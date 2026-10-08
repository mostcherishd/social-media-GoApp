package main

import (
	"bytes"
	"context"
	"html/template"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type fakeStore struct {
	mu       sync.Mutex
	profiles map[string]Profile
	objects  map[string][]byte
	types    map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{profiles: map[string]Profile{}, objects: map[string][]byte{}, types: map[string]string{}}
}

func (f *fakeStore) GetProfile(_ context.Context, username string) (*Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.profiles[username]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

func (f *fakeStore) SaveProfile(_ context.Context, p Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles[p.Username] = p
	return nil
}

func (f *fakeStore) UploadPicture(_ context.Context, key, contentType string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = body
	f.types[key] = contentType
	return nil
}

func (f *fakeStore) PictureURL(_ context.Context, key string) (string, error) {
	return "https://example-bucket.s3.amazonaws.com/" + key + "?X-Amz-Signature=abc&X-Amz-Expires=3600", nil
}

// 1x1 transparent PNG.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// setup points the package globals at templates and a fresh fake store.
func setup(t *testing.T) (http.Handler, *fakeStore) {
	t.Helper()
	var err error
	tmpl, err = template.ParseGlob("templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	fake := newFakeStore()
	store = fake
	hostname, serverIP, iamRole = "test-host", "10.0.0.1", "test-role"
	return newMux(), fake
}

func login(t *testing.T, h http.Handler, username, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/profile" {
		t.Fatalf("login: got %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session_token" {
			return c
		}
	}
	t.Fatal("login: no session cookie")
	return nil
}

func profileRequest(t *testing.T, cookie *http.Cookie, email, location, filename string, file []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("email", email)
	mw.WriteField("location", location)
	if file != nil {
		fw, err := mw.CreateFormFile("picture", filename)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(file)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/profile", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	return req
}

func TestLoginRejectsBadPassword(t *testing.T) {
	h, _ := setup(t)
	form := url.Values{"username": {"alex"}, "password": {"nope"}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Invalid username or password.") {
		t.Fatalf("got %d, body missing error", rec.Code)
	}
}

func TestProfileRequiresLogin(t *testing.T) {
	h, _ := setup(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/profile", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("got %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCompleteProfileFlow(t *testing.T) {
	h, fake := setup(t)
	cookie := login(t, h, "alex", "password123")

	// Fresh user sees the incomplete form.
	req := httptest.NewRequest(http.MethodGet, "/profile", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Profile incomplete") {
		t.Fatalf("initial profile page: %d", rec.Code)
	}

	// Save with a picture.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, profileRequest(t, cookie, "alex@example.com", "Lagos, Nigeria", "me.png", pngBytes))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/profile?saved=1" {
		t.Fatalf("save: got %d -> %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}

	p := fake.profiles["alex"]
	if p.Email != "alex@example.com" || p.Location != "Lagos, Nigeria" || p.Name != "Alex Morgan" {
		t.Fatalf("stored profile = %+v", p)
	}
	if !strings.HasPrefix(p.PictureKey, "profile-pictures/alex/") || !strings.HasSuffix(p.PictureKey, ".png") {
		t.Fatalf("picture key = %q", p.PictureKey)
	}
	if !bytes.Equal(fake.objects[p.PictureKey], pngBytes) || fake.types[p.PictureKey] != "image/png" {
		t.Fatalf("uploaded object mismatch (type %q)", fake.types[p.PictureKey])
	}

	// Profile page now shows the completed profile with the presigned picture URL.
	req = httptest.NewRequest(http.MethodGet, "/profile?saved=1", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"Profile complete", "Profile saved.", "alex@example.com", "Lagos, Nigeria", p.PictureKey + "?X-Amz-Signature=abc&amp;X-Amz-Expires=3600"} {
		if !strings.Contains(body, want) {
			t.Errorf("profile page missing %q", want)
		}
	}

	// Updating details without a new picture keeps the existing one.
	oldKey := p.PictureKey
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, profileRequest(t, cookie, "alex@new.example", "Abuja", "", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update without picture: %d: %s", rec.Code, rec.Body.String())
	}
	if p := fake.profiles["alex"]; p.PictureKey != oldKey || p.Location != "Abuja" {
		t.Fatalf("after update = %+v", p)
	}
}

func TestProfileValidation(t *testing.T) {
	h, fake := setup(t)
	cookie := login(t, h, "sam", "secure456")

	cases := []struct {
		name, email, location, filename string
		file                            []byte
		want                            string
	}{
		{"bad email", "not-an-email", "Paris", "me.png", pngBytes, "Please enter a valid email address."},
		{"missing location", "sam@example.com", "", "me.png", pngBytes, "Location is required."},
		{"first save needs picture", "sam@example.com", "Paris", "", nil, "Please choose a profile picture to upload."},
		{"not an image", "sam@example.com", "Paris", "evil.png", []byte("<script>alert(1)</script>"), "Picture must be a JPEG, PNG, GIF or WebP image."},
		{"too large", "sam@example.com", "Paris", "big.png", append(append([]byte{}, pngBytes...), make([]byte, maxPictureBytes)...), "Picture must be 5 MB or smaller."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, profileRequest(t, cookie, tc.email, tc.location, tc.filename, tc.file))
			if rec.Code < 400 || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("got %d, want error %q", rec.Code, tc.want)
			}
		})
	}

	if len(fake.profiles) != 0 || len(fake.objects) != 0 {
		t.Fatalf("nothing should be stored on validation failure: %d profiles, %d objects", len(fake.profiles), len(fake.objects))
	}
}

func TestLogoutEndsSession(t *testing.T) {
	h, _ := setup(t)
	cookie := login(t, h, "alex", "password123")

	req := httptest.NewRequest(http.MethodGet, "/logout", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/profile", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after logout, got %d", rec.Code)
	}
}

func TestRoleNameFromARN(t *testing.T) {
	cases := map[string]string{
		"arn:aws:sts::123456789012:assumed-role/devscale-app-role/i-0abc123":      "devscale-app-role",
		"arn:aws:sts::123456789012:assumed-role/MyRole/devscale-social-media-app": "MyRole",
		"arn:aws:iam::123456789012:user/daps":                                     "",
	}
	for arn, want := range cases {
		if got := roleNameFromARN(arn); got != want {
			t.Errorf("roleNameFromARN(%q) = %q, want %q", arn, got, want)
		}
	}
}
