package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/redis/go-redis/v9"
)

// Prerender loads URL in headless Chrome and returns final HTML after JS rendering.
func Prerender(ctx context.Context, url string, timeout time.Duration) (string, error) {
	// Create allocator - shared between requests, saves overhead.
	// In production, keep this as a global singleton, not per-request.
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx,
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)...,
	)
	defer cancelAlloc()

	// New browser context (= new tab).
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// Timeout for entire render.
	taskCtx, cancelTask := context.WithTimeout(browserCtx, timeout)
	defer cancelTask()

	var html string
	err := chromedp.Run(taskCtx,
		// Block unnecessary resources - faster render.
		network.Enable(),
		//network.SetBlockedURLs().URLPatterns([]string{
		//	"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp",
		//	"*.woff", "*.woff2", "*.ttf",
		//	"*google-analytics*", "*googletagmanager*",
		//}),
		chromedp.Navigate(url),
		// Wait until page settles. WaitReady waits for DOMContentLoaded+.
		chromedp.WaitReady("body", chromedp.ByQuery),
		// Extra pause for SPA - give JS time to render.
		// Better to wait for specific element if you know what to expect.
		chromedp.Sleep(500*time.Millisecond),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)
	if err != nil {
		return "", fmt.Errorf("prerender failed: %w", err)
	}
	return html, nil
}

// RemoveScriptTags removes all <script> and <noscript> tags from HTML.
func RemoveScriptTags(html string) string {
	// Remove <script> tags (including inline scripts)
	scriptRegex := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRegex.ReplaceAllString(html, "")

	// Remove <noscript> tags
	noscriptRegex := regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`)
	html = noscriptRegex.ReplaceAllString(html, "")

	return html
}

var redisClient *redis.Client
var cacheTTL time.Duration
var allowedDomains []string

// InitConfig initializes configuration from ENV variables.
func InitConfig() {
	// Load allowed domains
	allowedDomainsEnv := os.Getenv("ALLOWED_DOMAINS")
	if allowedDomainsEnv != "" {
		allowedDomains = strings.Split(allowedDomainsEnv, ",")
		for i := range allowedDomains {
			allowedDomains[i] = strings.TrimSpace(allowedDomains[i])
		}
		log.Printf("Allowed domains: %v", allowedDomains)
	} else {
		log.Println("ALLOWED_DOMAINS not set, all domains allowed")
	}
}

// InitRedis initializes Redis client from ENV variables.
func InitRedis() {
	redisAddr := os.Getenv("REDIS_ADDR")
	redisPassword := os.Getenv("REDIS_PASSWORD")
	redisDB := os.Getenv("REDIS_DB")
	cacheTTLStr := os.Getenv("CACHE_TTL")

	if redisAddr == "" {
		log.Println("REDIS_ADDR not set, cache disabled")
		return
	}

	db := 0
	if redisDB != "" {
		var err error
		db, err = strconv.Atoi(redisDB)
		if err != nil {
			log.Printf("invalid REDIS_DB value: %v, using 0", err)
		}
	}

	cacheTTL = 24 * time.Hour // default 24h
	if cacheTTLStr != "" {
		ttl, err := strconv.Atoi(cacheTTLStr)
		if err == nil {
			cacheTTL = time.Duration(ttl) * time.Second
		}
	}

	redisClient = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPassword,
		DB:       db,
	})

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Printf("Redis connection failed: %v, cache disabled", err)
		redisClient = nil
	} else {
		log.Printf("Redis cache enabled at %s with TTL %v", redisAddr, cacheTTL)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	// Get URL either from query parameter or from path
	target := r.URL.Query().Get("url")
	if target == "" {
		// Try path (e.g. /https://example.com)
		target = r.URL.Path
		if target != "" && target[0] == '/' {
			target = target[1:] // remove leading /
		}
	}

	if target == "" {
		http.Error(w, "missing URL (use ?url= or /https://example.com)", http.StatusBadRequest)
		return
	}

	// Add https:// if no scheme is present
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "https://" + target
	}

	log.Printf("Processing URL: %s", target)

	// Check allowed domains
	if len(allowedDomains) > 0 {
		parsedURL, err := url.Parse(target)
		if err != nil {
			http.Error(w, "invalid URL", http.StatusBadRequest)
			return
		}

		allowed := false
		hostname := parsedURL.Hostname()
		for _, domain := range allowedDomains {
			if hostname == domain || strings.HasSuffix(hostname, "."+domain) {
				allowed = true
				break
			}
		}

		if !allowed {
			log.Printf("Domain %s not allowed", hostname)
			http.Error(w, "domain not allowed", http.StatusForbidden)
			return
		}
	}

	var html string

	// Try cache if Redis is active
	if redisClient != nil {
		cached, err := redisClient.Get(r.Context(), target).Result()
		if err == nil {
			log.Printf("cache HIT for %s", target)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Prerender-Cache", "HIT")
			fmt.Fprint(w, cached)
			return
		}
		log.Printf("cache MISS for %s", target)
	}

	// Cache miss or no Redis - render
	var err error
	html, err = Prerender(r.Context(), target, 30*time.Second)
	if err != nil {
		log.Printf("prerender error for %s: %v", target, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Remove script tags from rendered HTML
	html = RemoveScriptTags(html)

	// Save to cache
	if redisClient != nil {
		if err := redisClient.Set(r.Context(), target, html, cacheTTL).Err(); err != nil {
			log.Printf("failed to cache %s: %v", target, err)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Prerender-Cache", "MISS")
	fmt.Fprint(w, html)
}

func main() {
	InitConfig()
	InitRedis()
	http.HandleFunc("/", handler)
	log.Println("prerender listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
