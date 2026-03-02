# 🗞️ Gazette

Gazette is a lightweight Gmail-to-RSS bridge designed specifically for reading newsletters in standard RSS readers (like NetNewsWire, Reeder, or FreshRSS). It polls your Gmail for newsletters, strips out the tracking scripts and complex CSS, and serves them as a clean, readable feed.

## ✨ Features

- **RSS & GReader API Support:** Compatible with both standard RSS readers and apps that support the Google Reader API (like NetNewsWire).
- **Clean Content:** Uses a semantic HTML parser to strip layouts, styles, and scripts while preserving text and images.
- **Gmail Sync:** Marking an item as read in your RSS reader removes the `UNREAD` label from the original message in Gmail.
- **Efficient Polling:** Background worker with configurable intervals and rate-limit handling.
- **Docker Ready:** Simple deployment with Docker and Docker Compose.

## 🚀 Getting Started

### 0. Prerequisites
- A publicly resolvable domain (e.g., `gazette.yourdomain.com`).
- An SSL certificate (Gazette is intended to run behind a reverse proxy like Nginx, Caddy, or Traefik that handles HTTPS). **Google OAuth will not work without a public HTTPS endpoint.**

### 1. Gmail API Setup

1.  Go to the [Google Cloud Console](https://console.cloud.google.com/).
2.  Create a new project and enable the **Gmail API**.
3.  Configure the **OAuth Consent Screen** (Internal or External).
4.  Create **OAuth 2.0 Client IDs** (Web application).
    -   Add `https://your-public-domain.com/auth/callback` to the **Authorized redirect URIs**.
5.  Download your Client ID and Client Secret.

### 2. Configuration

Create a `config.yaml` in the project root:

```yaml
gmail:
  client_id: "YOUR_CLIENT_ID"
  client_secret: "YOUR_CLIENT_SECRET"
  public_url: "https://gazette.yourdomain.com" # Must be HTTPS and publicly accessible
  label: "Newsletters"                          # Optional: Filter by a specific Gmail label
  polling_interval: 300                         # 5 minutes
  token_file: "/app/data/token.json"

server:
  port: 8080
  user: "admin"                       # For GReader ClientLogin
  pass: "password"
```

### 3. Run with Docker

```bash
docker-compose up -d
```

Once running, visit your configured `public_url` (e.g., `https://gazette.yourdomain.com`) to authorize the app with your Gmail account.

## 📖 How it Works

### RSS Feed
Individual feeds are available at `/feed?id=sender@example.com`.

### Google Reader API
Gazette implements a subset of the GReader API, making it a "source" for modern RSS clients. Point your reader's GReader/FreshRSS account to `https://gazette.yourdomain.com/api/greader.php/` and use the credentials defined in your `config.yaml`.

### Content Cleaning
Gazette doesn't just pass through the email HTML. It:
1.  **Strips CSS/JS:** Removes `<style>`, `<script>`, and `<head>` tags.
2.  **Whitelists Tags:** Only allows semantic tags like `<p>`, `<h1>-<h6>`, `<blockquote>`, `<code>`, and `<img>`.
3.  **Filters Noise:** Automatically removes common newsletter boilerplate like "Unsubscribe" or "View in Browser" links.
4.  **Preserves Full Content:** You can always click the article link in your reader to see the original, un-sanitized HTML via the `/view` endpoint.

## 🛠️ Development

-   **Language:** Go 1.25
-   **Storage:** Local JSON file (`/app/data/cache.json`) for persistence.
-   **Concurrency:** Thread-safe cache with `sync.RWMutex` and non-blocking Gmail polling.

---
*Note: Gazette is intended for personal use and is not a public multi-user service.*
