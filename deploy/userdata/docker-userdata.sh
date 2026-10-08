#!/bin/bash
set -euxo pipefail

# Logs for troubleshooting cloud-init/user-data execution
exec > >(tee /var/log/user-data.log | logger -t user-data -s 2>/dev/console) 2>&1

# -----------------------------
# REQUIRED: set your image here
# Example: DOCKER_IMAGE="mydockeruser/devscale-socialmedia:latest"
# -----------------------------
DOCKER_IMAGE="devscalecloud/devscale_social_media_app:v1"
CONTAINER_NAME="devscale-social-media-app"

# AWS resources: the S3 bucket and DynamoDB table (partition key "username", type String).
# The instance must have the IAM role from deploy/iam/ attached (IMDSv2, hop limit 2 for Docker);
# the AWS SDK picks up the role credentials automatically.
AWS_REGION="us-east-1"
S3_BUCKET="REPLACE_WITH_YOUR_BUCKET"
DYNAMODB_TABLE="devscale-profiles"

install_docker() {
  if command -v docker >/dev/null 2>&1; then
    return
  fi

  if command -v dnf >/dev/null 2>&1; then
    dnf -y update
    dnf -y install docker
  elif command -v yum >/dev/null 2>&1; then
    yum -y update
    yum -y install docker
  elif command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -y
    apt-get install -y ca-certificates curl gnupg lsb-release
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
    chmod a+r /etc/apt/keyrings/docker.gpg
    . /etc/os-release
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" > /etc/apt/sources.list.d/docker.list
    apt-get update -y
    apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  else
    echo "Unsupported OS: could not install Docker" >&2
    exit 1
  fi
}

install_docker

systemctl enable docker
systemctl start docker
systemctl is-active --quiet docker

if [ "$DOCKER_IMAGE" = "REPLACE_WITH_YOUR_IMAGE" ]; then
  echo "Set DOCKER_IMAGE in this script before using it." >&2
  exit 1
fi

if [ "$S3_BUCKET" = "REPLACE_WITH_YOUR_BUCKET" ]; then
  echo "Set S3_BUCKET in this script before using it." >&2
  exit 1
fi

docker pull "$DOCKER_IMAGE"

docker rm -f "$CONTAINER_NAME" || true

docker run -d \
  --name "$CONTAINER_NAME" \
  --restart unless-stopped \
  -p 8080:8080 \
  -e PORT=8080 \
  -e AWS_REGION="$AWS_REGION" \
  -e S3_BUCKET="$S3_BUCKET" \
  -e DYNAMODB_TABLE="$DYNAMODB_TABLE" \
  "$DOCKER_IMAGE"

echo "Container is running. Test locally on instance: http://localhost:8080"
