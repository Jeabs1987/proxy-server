#!/bin/bash

# Ensure we are in the correct directory
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

echo "Setup VPN Farm for Proxy Server"
echo "-------------------------------"

if [ -f ".env" ]; then
    echo "Found existing .env file."
    read -p "Do you want to overwrite it? (y/N): " overwrite
    if [[ "$overwrite" != "y" ]] && [[ "$overwrite" != "Y" ]]; then
        echo "Skipping credential setup."
    else
        SET_ENV=1
    fi
else
    SET_ENV=1
fi

if [ "$SET_ENV" == "1" ]; then
    read -p "Enter PIA Username (pNNNNNNN): " PIA_USER
    read -s -p "Enter PIA Password: " PIA_PASS
    echo ""

    cat > .env <<EOF
PIA_USERNAME=$PIA_USER
PIA_PASSWORD=$PIA_PASS
EOF
    echo "Secrets saved to .env"
fi

echo "Starting Docker containers..."
docker compose up -d

echo "-------------------------------"
echo "VPN Containers status:"
docker compose ps
echo "-------------------------------"
echo "Done."
