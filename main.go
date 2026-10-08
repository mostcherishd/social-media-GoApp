package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const maxPictureBytes = 5 << 20 // 5 MB

// Allowed picture types, detected from the file bytes rather than trusting the client.
var allowedPictureTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

type User struct {
	Username string
	Password string
	Name     string
}

type Session struct {
	Username  string
	ExpiresAt time.Time
}

// Profile is the record stored in DynamoDB, keyed by username.
type Profile struct {
	Username   string `dynamodbav:"username"`
	Name       string `dynamodbav:"name"`
	Email      string `dynamodbav:"email"`
	Location   string `dynamodbav:"location"`
	PictureKey string `dynamodbav:"picture_key,omitempty"`
	UpdatedAt  string `dynamodbav:"updated_at"`
}

type ProfileData struct {
	CurrentUser string
	Username    string
	Email       string
	Location    string
	PictureURL  string
	UpdatedAt   string
	Complete    bool
	Error       string
	Success     string
	ServerIP    string
	Hostname    string
	IAMRole     string
}

type LoginData struct {
	Error    string
	ServerIP string
	Hostname string
}

// ProfileStore persists profile details and pictures (AWS in production, a fake in tests).
type ProfileStore interface {
	GetProfile(ctx context.Context, username string) (*Profile, error)
	SaveProfile(ctx context.Context, p Profile) error
	UploadPicture(ctx context.Context, key, contentType string, body []byte) error
	PictureURL(ctx context.Context, key string) (string, error)
}

var (
	users = map[string]User{
		"alex": {
			Username: "alex",
			Password: "password123",
			Name:     "Alex Morgan",
		},
		"sam": {
			Username: "sam",
			Password: "secure456",
			Name:     "Sam Carter",
		},
	}

	sessions = map[string]Session{}
	mu       sync.RWMutex
	tmpl     *template.Template
	store    ProfileStore
	hostname = "Unknown"
	serverIP = "Unavailable"
	iamRole  = ""
)

// ---------------------------------------------------------------------------
// AWS: S3 for pictures, DynamoDB for profile details
// ---------------------------------------------------------------------------

type awsStore struct {
	s3      *s3.Client
	presign *s3.PresignClient
	ddb     *dynamodb.Client
	bucket  string
	table   string
}

// loadAWSConfig uses the IAM role attached to the EC2 instance (or env/~/.aws
// credentials when running locally). Role credentials are fetched with IMDSv2
// only; the SDK never falls back to token-less IMDSv1.
func loadAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithEC2RoleCredentialOptions(func(o *ec2rolecreds.Options) {
			o.Client = imds.New(imds.Options{EnableFallback: aws.FalseTernary})
		}),
	)
}

// resolveIAMRole checks the credentials work and returns the role name the app runs as.
func resolveIAMRole(ctx context.Context, cfg aws.Config) (string, string, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", "", err
	}
	arn := aws.ToString(out.Arn)
	return roleNameFromARN(arn), arn, nil
}

// roleNameFromARN extracts "MyRole" from "arn:aws:sts::123456789012:assumed-role/MyRole/i-0abc".
func roleNameFromARN(arn string) string {
	_, rest, ok := strings.Cut(arn, ":assumed-role/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

func (s *awsStore) GetProfile(ctx context.Context, username string) (*Profile, error) {
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"username": &types.AttributeValueMemberS{Value: username},
		},
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, nil
	}

	var p Profile
	if err := attributevalue.UnmarshalMap(out.Item, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *awsStore) SaveProfile(ctx context.Context, p Profile) error {
	item, err := attributevalue.MarshalMap(p)
	if err != nil {
		return err
	}
	_, err = s.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      item,
	})
	return err
}

func (s *awsStore) UploadPicture(ctx context.Context, key, contentType string, body []byte) error {
	_, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(contentType),
	})
	return err
}

// PictureURL returns a short-lived presigned URL so the bucket can stay private.
func (s *awsStore) PictureURL(ctx context.Context, key string) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(time.Hour))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func resolveServerInfo() (string, string) {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "Unknown"
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		return host, "Unavailable"
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			ip = ip.To4()
			if ip == nil {
				continue
			}

			return host, ip.String()
		}
	}

	return host, "Unavailable"
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func getSessionUser(r *http.Request) (User, bool) {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return User{}, false
	}

	mu.RLock()
	session, ok := sessions[cookie.Value]
	mu.RUnlock()
	if !ok || time.Now().After(session.ExpiresAt) {
		return User{}, false
	}

	mu.RLock()
	user, exists := users[session.Username]
	mu.RUnlock()
	return user, exists
}

func render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("%s template error: %v", name, err)
	}
}

func validateProfile(email, location string) string {
	if email == "" {
		return "Email is required."
	}
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email || len(email) > 254 {
		return "Please enter a valid email address."
	}
	if location == "" {
		return "Location is required."
	}
	if len(location) > 100 {
		return "Location must be 100 characters or less."
	}
	return ""
}

// buildProfileData turns the stored profile (if any) into the page model.
func buildProfileData(ctx context.Context, user User, p *Profile) ProfileData {
	data := ProfileData{
		CurrentUser: user.Name,
		Username:    user.Username,
		ServerIP:    serverIP,
		Hostname:    hostname,
		IAMRole:     iamRole,
	}
	if p == nil {
		return data
	}

	data.Email = p.Email
	data.Location = p.Location
	data.Complete = p.Email != "" && p.Location != "" && p.PictureKey != ""
	if t, err := time.Parse(time.RFC3339, p.UpdatedAt); err == nil {
		data.UpdatedAt = t.Format("02 Jan 2006, 15:04")
	}
	if p.PictureKey != "" {
		if url, err := store.PictureURL(ctx, p.PictureKey); err != nil {
			log.Printf("picture url for %s: %v", user.Username, err)
		} else {
			data.PictureURL = url
		}
	}
	return data
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodGet {
		if _, ok := getSessionUser(r); ok {
			http.Redirect(w, r, "/profile", http.StatusSeeOther)
			return
		}
		render(w, http.StatusOK, "login.html", LoginData{ServerIP: serverIP, Hostname: hostname})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	mu.RLock()
	user, ok := users[username]
	mu.RUnlock()
	if !ok || subtle.ConstantTimeCompare([]byte(user.Password), []byte(password)) != 1 {
		render(w, http.StatusUnauthorized, "login.html", LoginData{
			Error:    "Invalid username or password.",
			ServerIP: serverIP,
			Hostname: hostname,
		})
		return
	}

	token, err := generateToken()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	mu.Lock()
	sessions[token] = Session{Username: username, ExpiresAt: time.Now().Add(24 * time.Hour)}
	mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})

	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

func profileHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := getSessionUser(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	ctx := r.Context()
	existing, err := store.GetProfile(ctx, user.Username)
	if err != nil {
		log.Printf("get profile %s: %v", user.Username, err)
		data := buildProfileData(ctx, user, nil)
		data.Error = "Could not load your profile right now. Please try again."
		render(w, http.StatusInternalServerError, "profile.html", data)
		return
	}

	if r.Method == http.MethodGet {
		data := buildProfileData(ctx, user, existing)
		if r.URL.Query().Get("saved") == "1" {
			data.Success = "Profile saved."
		}
		render(w, http.StatusOK, "profile.html", data)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Re-render the form with what the user typed plus an error message.
	fail := func(status int, msg, email, location string) {
		data := buildProfileData(ctx, user, existing)
		data.Email = email
		data.Location = location
		data.Error = msg
		render(w, status, "profile.html", data)
	}

	// Allow a little headroom over the picture limit for the other form fields.
	r.Body = http.MaxBytesReader(w, r.Body, maxPictureBytes+(1<<20))
	if err := r.ParseMultipartForm(maxPictureBytes); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(http.StatusRequestEntityTooLarge, "Picture must be 5 MB or smaller.", "", "")
			return
		}
		fail(http.StatusBadRequest, "Could not read the form. Please try again.", "", "")
		return
	}
	defer r.MultipartForm.RemoveAll()

	email := strings.TrimSpace(r.FormValue("email"))
	location := strings.TrimSpace(r.FormValue("location"))
	if msg := validateProfile(email, location); msg != "" {
		fail(http.StatusBadRequest, msg, email, location)
		return
	}

	pictureKey := ""
	if existing != nil {
		pictureKey = existing.PictureKey
	}

	file, _, err := r.FormFile("picture")
	switch {
	case err == nil:
		defer file.Close()
		buf, err := io.ReadAll(io.LimitReader(file, maxPictureBytes+1))
		if err != nil || len(buf) == 0 {
			fail(http.StatusBadRequest, "Could not read the picture. Please try again.", email, location)
			return
		}
		if len(buf) > maxPictureBytes {
			fail(http.StatusBadRequest, "Picture must be 5 MB or smaller.", email, location)
			return
		}
		contentType := http.DetectContentType(buf)
		ext, ok := allowedPictureTypes[contentType]
		if !ok {
			fail(http.StatusBadRequest, "Picture must be a JPEG, PNG, GIF or WebP image.", email, location)
			return
		}

		suffix, err := generateToken()
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		key := fmt.Sprintf("profile-pictures/%s/%d-%s%s", user.Username, time.Now().Unix(), suffix[:8], ext)
		if err := store.UploadPicture(ctx, key, contentType, buf); err != nil {
			log.Printf("upload picture %s: %v", user.Username, err)
			fail(http.StatusBadGateway, "Could not upload your picture. Please try again.", email, location)
			return
		}
		pictureKey = key
	case errors.Is(err, http.ErrMissingFile):
		if pictureKey == "" {
			fail(http.StatusBadRequest, "Please choose a profile picture to upload.", email, location)
			return
		}
	default:
		fail(http.StatusBadRequest, "Could not read the picture. Please try again.", email, location)
		return
	}

	profile := Profile{
		Username:   user.Username,
		Name:       user.Name,
		Email:      email,
		Location:   location,
		PictureKey: pictureKey,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := store.SaveProfile(ctx, profile); err != nil {
		log.Printf("save profile %s: %v", user.Username, err)
		fail(http.StatusBadGateway, "Could not save your profile. Please try again.", email, location)
		return
	}

	http.Redirect(w, r, "/profile?saved=1", http.StatusSeeOther)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil {
		mu.Lock()
		delete(sessions, cookie.Value)
		mu.Unlock()
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", loginHandler)
	mux.HandleFunc("/profile", profileHandler)
	mux.HandleFunc("/logout", logoutHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	return mux
}

func main() {
	var err error
	hostname, serverIP = resolveServerInfo()

	tmpl, err = template.ParseGlob("templates/*.html")
	if err != nil {
		log.Fatalf("failed to parse templates: %v", err)
	}

	region := os.Getenv("AWS_REGION")
	bucket := os.Getenv("S3_BUCKET")
	table := os.Getenv("DYNAMODB_TABLE")
	if region == "" || bucket == "" || table == "" {
		log.Fatal("AWS_REGION, S3_BUCKET and DYNAMODB_TABLE must be set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	// Fail fast if the instance has no IAM role attached (or it can't be reached).
	role, arn, err := resolveIAMRole(ctx, cfg)
	if err != nil {
		log.Fatalf("no usable AWS credentials; attach the IAM role to this EC2 instance: %v", err)
	}
	if role == "" {
		log.Printf("WARNING: running as %s, not an IAM role", arn)
		iamRole = arn
	} else {
		iamRole = role
	}

	s3Client := s3.NewFromConfig(cfg)
	store = &awsStore{
		s3:      s3Client,
		presign: s3.NewPresignClient(s3Client),
		ddb:     dynamodb.NewFromConfig(cfg),
		bucket:  bucket,
		table:   table,
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	fmt.Println("============================================")
	fmt.Println("  DevScale Social Media App")
	fmt.Println("============================================")
	fmt.Printf("  Server running at http://0.0.0.0%s\n", addr)
	fmt.Printf("  Hostname: %s\n", hostname)
	fmt.Printf("  IP Address: %s\n", serverIP)
	fmt.Printf("  IAM identity: %s\n", arn)
	fmt.Printf("  S3 bucket: %s\n", bucket)
	fmt.Printf("  DynamoDB table: %s\n", table)
	fmt.Println("  Open your browser: http://localhost:" + port)
	fmt.Println("  Demo login: alex / password123")
	fmt.Println("============================================")
	log.Fatal(http.ListenAndServe(addr, newMux()))
}
