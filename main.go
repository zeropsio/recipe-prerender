package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/redis/go-redis/v9"
)

type PageConfig struct {
	query      []string
	pathPrefix []string
}

func (pc PageConfig) Get(u *url.URL) (bool, string, error) {
	if len(pc.pathPrefix) > 0 {
		matched := false
		for _, prefix := range pc.pathPrefix {
			if strings.HasPrefix(u.Path, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			return false, "", nil
		}
	}

	newURL := &url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   u.Path,
	}

	if len(pc.query) > 0 {
		src := u.Query()
		filtered := url.Values{}
		for _, key := range pc.query {
			if vals, ok := src[key]; ok {
				filtered[key] = vals
			}
		}
		newURL.RawQuery = filtered.Encode()
	}

	return true, newURL.String(), nil
}

var blockedURLPatterns = []*network.BlockPattern{
	{URLPattern: "*://*:*/*.css"},
	{URLPattern: "*://*:*/*.ico"},
	{URLPattern: "*://*:*/*.png"},
	{URLPattern: "*://*:*/*.jpg"},
	{URLPattern: "*://*:*/*.jpeg"},
	{URLPattern: "*://*:*/*.gif"},
	{URLPattern: "*://*:*/*.webp"},
	{URLPattern: "*://*:*/*.woff"},
	{URLPattern: "*://*:*/*.woff2"},
	{URLPattern: "*://*:*/*.ttf"},
	{URLPattern: "*://*:*/*google-analytics*"},
	{URLPattern: "*://*:*/*googletagmanager*"},
}

type tab struct {
	ctx context.Context
}

type tabPool struct {
	ch chan *tab
}

func newTabPool(parentCtx context.Context, size int) (*tabPool, error) {
	pool := &tabPool{ch: make(chan *tab, size)}
	for i := 0; i < size; i++ {
		tabCtx, _ := chromedp.NewContext(parentCtx)
		if err := chromedp.Run(tabCtx,
			network.Enable(),
			network.SetBlockedURLs().WithURLPatterns(blockedURLPatterns),
		); err != nil {
			return nil, fmt.Errorf("tab %d init: %w", i, err)
		}
		pool.ch <- &tab{ctx: tabCtx}
	}
	return pool, nil
}

func (tp *tabPool) get(ctx context.Context) (*tab, error) {
	select {
	case t := <-tp.ch:
		return t, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (tp *tabPool) put(t *tab) {
	tp.ch <- t
}

type prerender struct {
	ctx           context.Context
	cancelAlloc   context.CancelFunc
	cancelBrowser context.CancelFunc

	token          string
	redisClient    *redis.Client
	cacheTTL       time.Duration
	allowedDomains []string

	domains map[string]PageConfig
	tabs    *tabPool
}

func newPrerender(ctx context.Context) (*prerender, error) {
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx,
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)...,
	)
	browserCtx, browserCancel := chromedp.NewContext(ctx)

	if err := chromedp.Run(browserCtx,
		// Block unnecessary resources - faster render.
		network.Enable(),
		//network.SetBlockedURLs().URLPatterns([]string{
		//	"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp",
		//	"*.woff", "*.woff2", "*.ttf",
		//	"*google-analytics*", "*googletagmanager*",
		//}),
		chromedp.Navigate("http://localhost:8080/status"),
		// Wait until page settles. WaitReady waits for DOMContentLoaded+.
		chromedp.WaitReady("body", chromedp.ByQuery),
		// Extra pause for SPA - give JS time to render.
		// Better to wait for specific element if you know what to expect.
		chromedp.Sleep(500*time.Millisecond),
	); err != nil {
		return nil, err
	}

	p := &prerender{
		ctx:           allocCtx,
		cancelAlloc:   cancelAlloc,
		cancelBrowser: browserCancel,
		domains: map[string]PageConfig{
			"app.zerops.io": {
				pathPrefix: []string{
					"/recipes",
				},
				query: []string{
					"environment",
					"guideFlow",
					"guideEnv",
					"guideApp",
				},
			},
		},
	}
	if err := p.initRedis(); err != nil {
		return nil, err
	}
	if err := p.initConfig(); err != nil {
		return nil, err
	}

	poolSize := 5
	if s := os.Getenv("TAB_POOL_SIZE"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			poolSize = n
		}
	}
	tabs, err := newTabPool(allocCtx, poolSize)
	if err != nil {
		return nil, err
	}
	p.tabs = tabs
	log.Printf("Tab pool initialized with %d tabs", poolSize)

	return p, nil
}

func (p *prerender) handle(targetURL string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()

	t, err := p.tabs.get(ctx)
	if err != nil {
		return "", fmt.Errorf("no tab available: %w", err)
	}
	defer p.tabs.put(t)

	deadline, _ := ctx.Deadline()
	renderCtx, cancelRender := context.WithDeadline(t.ctx, deadline)
	defer cancelRender()

	var html string
	if err := chromedp.Run(renderCtx,
		chromedp.Navigate(targetURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	); err != nil {
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

// InitConfig initializes configuration from ENV variables.
func (p *prerender) initConfig() error {
	// Load allowed domains
	p.token = os.Getenv("PRERENDER_TOKEN")
	if p.token != "" {
		log.Printf("Using token: %s", p.token)
	} else {
		log.Printf("Token not set")
	}

	allowedDomainsEnv := os.Getenv("ALLOWED_DOMAINS")
	if allowedDomainsEnv != "" {
		p.allowedDomains = strings.Split(allowedDomainsEnv, ",")
		for i := range p.allowedDomains {
			p.allowedDomains[i] = strings.TrimSpace(p.allowedDomains[i])
		}
		log.Printf("Allowed domains: %v", p.allowedDomains)
	} else {
		log.Println("ALLOWED_DOMAINS not set, all domains allowed")
	}
	return nil
}

// InitRedis initializes Redis client from ENV variables.
func (p *prerender) initRedis() error {
	redisAddr := os.Getenv("REDIS_ADDR")
	redisPassword := os.Getenv("REDIS_PASSWORD")
	redisDB := os.Getenv("REDIS_DB")
	cacheTTLStr := os.Getenv("CACHE_TTL")
	if redisAddr == "" {
		log.Println("REDIS_ADDR not set, cache disabled")
		return nil
	}

	db := 0
	if redisDB != "" {
		var err error
		db, err = strconv.Atoi(redisDB)
		if err != nil {
			log.Printf("invalid REDIS_DB value: %v, using 0", err)
		}
	}

	p.cacheTTL = 24 * time.Hour // default 24h
	if cacheTTLStr != "" {
		ttl, err := strconv.Atoi(cacheTTLStr)
		if err == nil {
			p.cacheTTL = time.Duration(ttl) * time.Second
		}
	}

	p.redisClient = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPassword,
		DB:       db,
	})

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.redisClient.Ping(ctx).Err(); err != nil {
		log.Printf("Redis connection failed: %v, cache disabled", err)
		p.redisClient = nil
	} else {
		log.Printf("Redis cache enabled at %s with TTL %v", redisAddr, p.cacheTTL)
	}
	return nil
}

func (p *prerender) handler(w http.ResponseWriter, r *http.Request) {

	if p.token != "" && r.Header.Get("X-Prerender-Token") != p.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Get URL either from query parameter or from path
	target := r.URL.String()
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

	target, err := url.QueryUnescape(target)
	if err != nil {
		fmt.Println("Error decoding query string: ", err.Error())
		return
	}

	target = strings.TrimPrefix(target, "/")

	// Add https:// if no scheme is present
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "https://" + target
	}

	log.Printf("Processing URL: %s", target)

	// Check allowed domains
	parsedURL, err := url.Parse(target)
	if err != nil {
		http.Error(w, "invalid URL", http.StatusBadRequest)
		return
	}

	if len(p.allowedDomains) > 0 {
		allowed := false
		hostname := parsedURL.Hostname()
		for _, domain := range p.allowedDomains {
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

	if site, exists := p.domains[parsedURL.Host]; exists {
		var match bool
		match, target, err = site.Get(parsedURL)
		if err != nil {
			log.Fatal("Error: %v", err)
			http.Error(w, "domain not allowed", http.StatusForbidden)
			return
		}
		if !match {
			log.Fatal("Domain not allowed: %v", err)
			http.Error(w, "Domain not allowed", http.StatusForbidden)
			return
		}
	}

	var html string

	// Try cache if Redis is active
	if p.redisClient != nil {
		cached, err := p.redisClient.Get(r.Context(), target).Result()
		if err == nil {
			log.Printf("cache HIT for %s / %s", target, target)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Prerender-Cache", "HIT")
			fmt.Fprint(w, cached)
			return
		}
		log.Printf("cache MISS for %s", target)
	}

	// Cache miss or no Redis - render
	html, err = p.handle(target, 30*time.Second)
	if err != nil {
		log.Printf("prerender error for %s: %v", target, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Remove script tags from rendered HTML
	html = RemoveScriptTags(html)

	// Save to cache
	if p.redisClient != nil {
		if err := p.redisClient.Set(r.Context(), target, html, p.cacheTTL).Err(); err != nil {
			log.Printf("failed to cache %s: %v", target, err)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Prerender-Cache", "MISS")
	fmt.Fprint(w, html)
}

type Router struct {
	prerender *prerender
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/status":
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
		return
	default:
		if r.prerender != nil {
			r.prerender.handler(w, req)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "NOT READY")
	}
}

func main() {
	if err := Run(func(ctx context.Context) error {
		httpRouter := &Router{}
		httpServer := http.Server{
			Addr:    ":8080",
			Handler: httpRouter,
		}

		go func() {
			if err := httpServer.ListenAndServe(); err != nil {
				log.Fatalf("failed to start HTTP server: %v", err)
			}
		}()

		p, err := newPrerender(ctx)
		if err != nil {
			return err
		}
		defer p.cancelAlloc()
		httpRouter.prerender = p
		<-ctx.Done()
		return nil
	}); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}

}

func RunWithContext(ctx context.Context, callback func(context.Context) error) error {
	return callback(ContextWithSigterm(ctx))
}

func Run(callback func(context.Context) error) error {
	return RunWithContext(context.Background(), callback)
}

func ContextWithSigterm(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt,
			os.Interrupt,
			syscall.SIGTERM,
			syscall.SIGQUIT,
		)
		<-interrupt
		cancel()
	}()
	return ctx
}
