package main

// AppConfig holds every setting that changes between deployments. All values
// come from the environment at startup; see .env.example for the full list.

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

type AppConfig struct {
	// Host the messenger is served from, without a scheme.
	// Example: chat.example.com
	Domain string

	// TLS certificate and key paths.
	CertFile string
	KeyFile  string

	// Product name shown in the UI.
	AppName string

	// Admin panel origin (https://…), used as the CORS origin for /admin/*.
	AdminOrigin string

	// TURN server URLs.
	// Example: turn:turn.example.com:3478,turns:turn.example.com:5349
	TurnURLs []string

	// Contact address for VAPID push.
	VapidContact string

	// Google OAuth client id; empty hides the Google sign-in button.
	GoogleClientID string
}

func loadConfig() *AppConfig {
	domain := getEnvOrDefault("APP_DOMAIN", "localhost")
	certDir := "/etc/letsencrypt/live/" + domain

	// Offer UDP *and* TCP transports. Mobile carriers and public/corporate
	// Wi-Fi frequently block UDP or non-standard ports; a TURN/TCP relay gets
	// through many of those. The real firewall-buster is turns over TCP/443
	// (indistinguishable from HTTPS), but that needs coturn to own port 443 —
	// a separate IP/subdomain from the web server. When that's set up, point
	// TURN_URLS at it (e.g. "turns:turn.example.com:443?transport=tcp,...").
	turnURLs := []string{
		"turn:" + domain + ":3478?transport=udp",
		"turn:" + domain + ":3478?transport=tcp",
		"turns:" + domain + ":5349?transport=tcp",
	}
	if custom := os.Getenv("TURN_URLS"); custom != "" {
		turnURLs = splitTrim(custom, ",")
	}

	return &AppConfig{
		Domain:   domain,
		CertFile: getEnvOrDefault("TLS_CERT", certDir+"/fullchain.pem"),
		KeyFile:  getEnvOrDefault("TLS_KEY", certDir+"/privkey.pem"),
		AppName:  getEnvOrDefault("APP_NAME", "Hexeris"),
		// An empty ADMIN_ORIGIN means no Access-Control-Allow-Origin header,
		// so cross-origin calls to /admin/* are blocked. The safe default is
		// "off" rather than someone else's domain.
		AdminOrigin:    getEnvOrDefault("ADMIN_ORIGIN", ""),
		TurnURLs:       turnURLs,
		VapidContact:   getEnvOrDefault("VAPID_CONTACT", "mailto:admin@"+domain),
		GoogleClientID: os.Getenv("GOOGLE_CLIENT_ID"),
	}
}

// getEnvInt and getEnvBool never fail the startup on a malformed value: a
// typo in .env should not turn performance tuning into an outage.
func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		log.Printf("config: %s=%q is not a positive number, using %d", key, v, def)
		return def
	}
	return n
}

// getEnvDurationSeconds reads a duration expressed in whole seconds.
func getEnvDurationSeconds(key string, defSecs int) time.Duration {
	return time.Duration(getEnvInt(key, defSecs)) * time.Second
}

func getEnvBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("config: %s=%q is not a boolean, using %v", key, v, def)
		return def
	}
	return b
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitTrim(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
