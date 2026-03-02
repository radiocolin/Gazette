package main

import (
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func registerAuthHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/auth/login", handleOAuthLogin)
	mux.HandleFunc("/auth/callback", handleOAuthCallback)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		return
	}
	
	gmailMu.RLock()
	svc := gmailSvc
	gmailMu.RUnlock()

	w.Header().Set("Content-Type", "text/html")
	if svc != nil {
		fmt.Fprintf(w, `
			<html>
			<head><style>body{font-family:sans-serif; line-height:1.5; max-width:800px; margin:40px auto; padding:0 20px; color:#333;}</style></head>
			<body>
				<h1>GazetteBridge Status: <span style="color:green">Active</span></h1>
				<p>Connected to Gmail. Polling for label: <b>%s</b></p>
				<hr>
				<h3>NetNewsWire Setup</h3>
				<p>Add a new <b>FreshRSS</b> account with these details:</p>
				<ul>
					<li><b>Server:</b> %s</li>
					<li><b>Password:</b> (configured in config.yaml)</li>
				</ul>
			</body></html>`, config.Gmail.Label, config.Gmail.PublicURL)
		return
	}

	callbackURL := strings.TrimSuffix(config.Gmail.PublicURL, "/") + "/auth/callback"
	fmt.Fprintf(w, `
		<html>
		<head><style>body{font-family:sans-serif; line-height:1.5; max-width:800px; margin:40px auto; padding:0 20px; color:#333;} code{background:#eee; padding:2px 4px; border-radius:3px;}</style></head>
		<body>
			<h1>GazetteBridge Setup</h1>
			<p>Follow these steps to connect your Gmail account:</p>
			
			<ol>
				<li>Go to the <a href="https://console.cloud.google.com/" target="_blank">Google Cloud Console</a>.</li>
				<li>Create a new project (e.g., "GazetteBridge").</li>
				<li>Search for <b>"Gmail API"</b> and click <b>Enable</b>.</li>
				<li>Go to <b>APIs & Services > OAuth consent screen</b>:
					<ul>
						<li>Select <b>External</b>.</li>
						<li>Fill in the app name and your email.</li>
						<li>Add the scope: <code>.../auth/gmail.modify</code> (allows marking as read).</li>
						<li>Add your email as a <b>Test User</b> (required while in "Testing" mode).</li>
					</ul>
				</li>
				<li>Go to <b>APIs & Services > Credentials</b>:
					<ul>
						<li>Click <b>Create Credentials > OAuth client ID</b>.</li>
						<li>Select <b>Web application</b>.</li>
						<li>Add this <b>Authorized redirect URI</b>:<br><code>%s</code></li>
					</ul>
				</li>
				<li>Copy the <b>Client ID</b> and <b>Client Secret</b> into your <code>config.yaml</code>.</li>
				<li>Restart this container.</li>
			</ol>

			<div style="margin-top:30px; padding:20px; background:#f8f9fa; border:1px solid #dee2e6; border-radius:5px;">
				<p>Once your Client ID and Secret are in <code>config.yaml</code>, click below:</p>
				<a href="/auth/login" style="display:inline-block; padding: 10px 20px; background: #4285F4; color: white; text-decoration: none; border-radius: 5px; font-weight:bold;">Authorize with Google</a>
			</div>
		</body></html>`, callbackURL)
}

func handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if oauthConf == nil {
		http.Error(w, "OAuth not configured. Check your config.yaml or environment variables.", http.StatusInternalServerError)
		return
	}
	url := oauthConf.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "No code in callback", http.StatusBadRequest)
		return
	}

	tok, err := oauthConf.Exchange(r.Context(), code)
	if err != nil {
		http.Error(w, fmt.Sprintf("Token exchange failed: %v", err), http.StatusInternalServerError)
		return
	}

	saveToken(config.Gmail.TokenFile, tok)

	svc, err := gmail.NewService(r.Context(), option.WithHTTPClient(oauthConf.Client(r.Context(), tok)))
	if err != nil {
		http.Error(w, fmt.Sprintf("Service init failed: %v", err), http.StatusInternalServerError)
		return
	}

	gmailMu.Lock()
	gmailSvc = svc
	gmailMu.Unlock()

	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}
