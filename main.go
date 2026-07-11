package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

type config struct {
	BaseURL        string
	Port           string
	MetricsPath    string
	ScrapeInterval time.Duration
	Email          string
	Password       string
	FlareSolverURL string
}

func loadConfig() config {
	base := getEnvOrDefault("YGGREBORN_API_BASE_URL", "https://www.yggreborn.org")
	return config{
		BaseURL:        base,
		Port:           getEnvOrDefault("PORT", "9090"),
		MetricsPath:    getEnvOrDefault("METRICS_PATH", "/metrics"),
		Email:          os.Getenv("YGGREBORN_EMAIL"),
		Password:       os.Getenv("YGGREBORN_PASSWORD"),
		ScrapeInterval: parseDuration(os.Getenv("SCRAPE_INTERVAL"), 5*time.Minute),
		FlareSolverURL: getEnvOrDefault("FLARESOLVER_URL", "http://localhost:8191"),
	}
}

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// -----------------------------------------------------------------------------
// FlareSolverr — get cf_clearance cookie
// -----------------------------------------------------------------------------

type flareRequest struct {
	Cmd          string            `json:"cmd"`
	URL          string            `json:"url"`
	PostData     string            `json:"postData,omitempty"`
	Cookies      []map[string]string `json:"cookies,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	QueryString  *struct{ Fields map[string]string } `json:"queryString,omitempty"`
	ReturnRaw    bool              `json:"returnRaw,omitempty"`
	MaxTimeout   int               `json:"maxTimeout,omitempty"`
}

type flareResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Response struct {
		StatusCode int            `json:"statusCode"`
		Headers    map[string]string `json:"headers"`
		Raw        string `json:"raw,omitempty"`
	} `json:"response,omitempty"`
	Solution struct {
		Cookies []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
			Path   string `json:"path"`
		} `json:"cookies"`
		UserAgent string `json:"userAgent"`
		Response  string `json:"response"`
	} `json:"solution"`
}

func flareClient() *http.Client {
	return &http.Client{Timeout: 120 * time.Second}
}

func flareEndpoint(baseURL string) string {
	return baseURL + "/v1"
}

func doFlare(flareSolverURL string, req flareRequest) (*flareResponse, error) {
	url := flareEndpoint(flareSolverURL)
	payload, _ := json.Marshal(req)
	client := flareClient()
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("flare request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result flareResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse flare response: %w\n%s", err, string(body))
	}
	if result.Status != "ok" {
		return nil, fmt.Errorf("flare error: %s — %s", result.Status, result.Message)
	}
	return &result, nil
}

// waitForFlareSolverr polls until FlareSolverr responds
func waitForFlareSolverr(flareSolverURL string) error {
	url := flareEndpoint(flareSolverURL)
	fmt.Printf("[flare] Waiting for FlareSolverr at %s ...\n", url)
	client := flareClient()
	for i := 0; i < 60; i++ {
		resp, err := client.Post(url, "application/json",
			bytes.NewReader([]byte(`{"cmd":"request.get","url":"http://www.google.com/","maxTimeout":60000}`)))
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			fmt.Println("[flare] FlareSolverr is ready")
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("FlareSolverr not ready after 120s")
}

// getFlareCookies visits the login page via FlareSolverr and returns ALL cookies + UA
func getFlareCookies(flareSolverURL, baseURL string) ([]*http.Cookie, string, error) {
	fmt.Printf("[flare] Getting cookies via FlareSolverr ...\n")
	res, err := doFlare(flareSolverURL, flareRequest{
		Cmd: "request.get",
		URL: baseURL + "/login",
	})
	if err != nil {
		return nil, "", fmt.Errorf("flare cookies: %w", err)
	}

	var cookies []*http.Cookie
	var cfLen int
	for _, c := range res.Solution.Cookies {
		cookie := &http.Cookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: c.Domain,
			Path:   c.Path,
		}
		cookies = append(cookies, cookie)
		if c.Name == "cf_clearance" {
			cfLen = len(c.Value)
		}
	}

	fmt.Printf("[flare] Got %d cookies (cf_clearance len=%d), UA: %s\n", len(cookies), cfLen, res.Solution.UserAgent)
	return cookies, res.Solution.UserAgent, nil
}

// -----------------------------------------------------------------------------
// YggRebornClient — direct HTTP with cf_clearance cookie
// -----------------------------------------------------------------------------

type YggRebornClient struct {
	mu            sync.RWMutex
	baseURL       string
	flareSolverURL string
	userAgent     string
	flareCookies  []*http.Cookie // cookies from FlareSolverr (cf_clearance + session)
	sessionCookies []*http.Cookie // cookies from login response
	authenticated bool
	httpClient    *http.Client
	loginClient   *http.Client
}

// allCookies returns flareCookies + sessionCookies combined
func (c *YggRebornClient) allCookies() []*http.Cookie {
	c.mu.RLock()
	defer c.mu.RUnlock()
	combined := make([]*http.Cookie, 0, len(c.flareCookies)+len(c.sessionCookies))
	combined = append(combined, c.flareCookies...)
	combined = append(combined, c.sessionCookies...)
	return combined
}

func newYggRebornClient(baseURL string) *YggRebornClient {
	c := &YggRebornClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
	c.loginClient = &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Capture Set-Cookie headers from the 302 redirect response
			if len(via) > 0 {
				c.mu.Lock()
				c.sessionCookies = parseSetCookies(via[len(via)-1].Response.Header["Set-Cookie"])
				c.mu.Unlock()
			}
			return nil // follow redirect
		},
	}
	return c
}

func (c *YggRebornClient) IsAuthenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.authenticated
}

// extractCSRFToken parses the login page HTML and returns the csrf_token value
func extractCSRFToken(html []byte) (string, error) {
	re := regexp.MustCompile(`<input\s+type="hidden"\s+name="csrf_token"\s+value="([^"]+)"`)
	matches := re.FindSubmatch(html)
	if len(matches) < 2 {
		return "", fmt.Errorf("no csrf_token found in login page")
	}
	return string(matches[1]), nil
}

// parseSetCookies extracts cookies from Set-Cookie headers
func parseSetCookies(headers []string) []*http.Cookie {
	var cookies []*http.Cookie
	for _, sc := range headers {
		idx := strings.IndexByte(sc, '=')
		if idx > 0 {
			name := sc[:idx]
			valueEnd := strings.Index(sc[idx+1:], ";")
			var value string
			if valueEnd > 0 {
				value = sc[idx+1 : idx+1+valueEnd]
			} else {
				value = sc[idx+1:]
			}
			cookies = append(cookies, &http.Cookie{Name: name, Value: value})
		}
	}
	return cookies
}

// flarePostRequest is the request body for FlareSolverr request.post
func flarePostRequest(baseURL, postURL string, cookies []*http.Cookie, formData string) flareRequest {
	cookieMaps := make([]map[string]string, 0, len(cookies))
	for _, c := range cookies {
		cookieMaps = append(cookieMaps, map[string]string{"name": c.Name, "value": c.Value})
	}
	return flareRequest{
		Cmd:          "request.post",
		URL:          postURL,
		ReturnRaw:    true,
		MaxTimeout:   60000,
		PostData:     formData,
		Headers:      map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		Cookies:      cookieMaps,
	}
}

// doFlarePost sends a POST request via FlareSolverr (real browser)
func doFlarePost(baseURL, postURL string, cookies []*http.Cookie, formData string) (*flareResponse, error) {
	url := flareEndpoint(baseURL)
	payload, _ := json.Marshal(flarePostRequest(baseURL, postURL, cookies, formData))
	client := flareClient()
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("flare post: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result flareResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse flare response: %w, body: %s", err, string(body))
	}
	return &result, nil
}

// Login uses FlareSolverr (real browser) to submit the login form
func (c *YggRebornClient) Login(identifier, password string) error {
	fmt.Printf("[auth] Logging in: %s/login\n", c.baseURL)

	// Step 1: GET /login with flare cookies to extract fresh CSRF token
	getReq, err := http.NewRequest(http.MethodGet, c.baseURL+"/login", nil)
	if err != nil {
		return fmt.Errorf("build login GET request: %w", err)
	}
	getReq.Header.Set("User-Agent", c.userAgent)
	for _, ck := range c.allCookies() {
		getReq.AddCookie(ck)
	}

	resp, err := c.httpClient.Do(getReq)
	if err != nil {
		return fmt.Errorf("login page request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login page returned status %d", resp.StatusCode)
	}

	csrfToken, err := extractCSRFToken(body)
	if err != nil {
		return fmt.Errorf("extract csrf: %w", err)
	}

	// Capture session cookies from the GET /login response.
	// Flask ties the CSRF token to the session cookie it sets.
	getCookies := parseSetCookies(resp.Header["Set-Cookie"])
	fmt.Printf("[auth] Got %d cookie(s) from GET /login\n", len(getCookies))

	// Step 2: POST /login via FlareSolverr (real browser) to handle Turnstile/JS challenges
	formData := url.Values{
		"identifier": {identifier},
		"password":   {password},
		"csrf_token": {csrfToken},
	}

	// Combine flare cookies (cf_clearance) with GET response cookies (__ygg_sess)
	allPostCookies := make([]*http.Cookie, 0, len(c.flareCookies)+len(getCookies))
	for _, ck := range c.flareCookies {
		allPostCookies = append(allPostCookies, ck)
	}
	for _, ck := range getCookies {
		allPostCookies = append(allPostCookies, ck)
	}

	loginResult, err := doFlarePost(c.flareSolverURL, c.baseURL+"/login", allPostCookies, formData.Encode())
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}

	// Check if the response is still the login page (failure)
	loginHTML := loginResult.Solution.Response
	if strings.Contains(loginHTML, "<title>YggReborn | Connexion</title>") &&
		!strings.Contains(loginHTML, "<title>YggReborn | Mon compte</title>") {
		return fmt.Errorf("login failed — returned to login page")
	}

	// Capture session cookies from FlareSolverr response
	var newSessionCookies []*http.Cookie
	for _, c := range loginResult.Solution.Cookies {
		if c.Name == "__ygg_sess" {
			newSessionCookies = append(newSessionCookies, &http.Cookie{
				Name:   c.Name,
				Value:  c.Value,
				Domain: c.Domain,
				Path:   c.Path,
			})
		}
	}

	c.mu.Lock()
	c.sessionCookies = newSessionCookies
	c.authenticated = true
	c.mu.Unlock()

	fmt.Println("[auth] Login successful")
	return nil
}

// FetchMetrics GETs /account and parses HTML for upload/download stats
func (c *YggRebornClient) FetchMetrics() (int64, int64, error) {
	c.mu.RLock()
	auth := c.authenticated
	c.mu.RUnlock()
	if !auth {
		return 0, 0, fmt.Errorf("not authenticated")
	}

	// Build cookies list from flare + session
	allCookies := make([]*http.Cookie, 0, len(c.flareCookies)+len(c.sessionCookies))
	for _, ck := range c.flareCookies {
		allCookies = append(allCookies, ck)
	}
	for _, ck := range c.sessionCookies {
		allCookies = append(allCookies, ck)
	}
	cookieMaps := make([]map[string]string, 0, len(allCookies))
	for _, ck := range allCookies {
		cookieMaps = append(cookieMaps, map[string]string{"name": ck.Name, "value": ck.Value})
	}

	// Use FlareSolverr GET to maintain real browser context
	req := flareRequest{
		Cmd:     "request.get",
		URL:     c.baseURL + "/account",
		Cookies: cookieMaps,
	}
	result, err := doFlare(c.flareSolverURL, req)
	if err != nil {
		return 0, 0, fmt.Errorf("metrics request: %w", err)
	}

	// Get response body from FlareSolverr
	accountHTML := result.Solution.Response
	fmt.Printf("[metrics] Response: %d bytes\n", len(accountHTML))

	// Check if we got the login page (not authenticated)
	if strings.Contains(accountHTML, "<title>YggReborn | Connexion</title>") {
		return 0, 0, fmt.Errorf("unauthorized — got login page")
	}

	uploaded, downloaded, err := parseAccountPage([]byte(accountHTML))
	if err != nil {
		return 0, 0, fmt.Errorf("parse account page: %w", err)
	}

	return uploaded, downloaded, nil
}

// parseAccountPage extracts upload/download values from the hero-card HTML
func parseAccountPage(html []byte) (int64, int64, error) {
	// Strategy: find the "Statistiques Tracker" card, then parse the first
	// two stat blocks (Upload and Download) for their human-readable values.
	//
	// HTML structure:
	//   <div class="hero-card ...">
	//     <h3 ...>Statistiques Tracker</h3>
	//     ...
	//     <div class="grid grid-cols-2 gap-4">
	//       <div class="rounded-lg p-3 text-center" style="background: rgba(102,221,183,...">
	//         <div class="text-xl sm:text-2xl font-extrabold text-mint-400 truncate">282.82 Go</div>
	//         <div ...>Upload</div>
	//       </div>
	//       <div class="rounded-lg p-3 text-center" style="background: rgba(239,68,68,...">
	//         <div class="text-xl sm:text-2xl font-extrabold text-red-400 truncate">6.33 Go</div>
	//         <div ...>Download</div>
	//       </div>
	//       ...
	//     </div>
	//   </div>
	//
	// We look for the Upload block (mint-400 color) and Download block (red-400 color).

	uploadRe := regexp.MustCompile(`<div class="text-xl sm:text-2xl font-extrabold text-mint-400[^"]*">([^<]+)</div>\s*<div[^>]*>Upload</div>`)
	downloadRe := regexp.MustCompile(`<div class="text-xl sm:text-2xl font-extrabold text-red-400[^"]*">([^<]+)</div>\s*<div[^>]*>Download</div>`)

	uploadMatch := uploadRe.FindSubmatch(html)
	downloadMatch := downloadRe.FindSubmatch(html)

	if uploadMatch == nil || downloadMatch == nil {
		return 0, 0, fmt.Errorf("could not find upload/download stats in HTML")
	}

	uploadStr := strings.TrimSpace(string(uploadMatch[1]))
	downloadStr := strings.TrimSpace(string(downloadMatch[1]))

	uploadedBytes, err := parseSize(uploadStr)
	if err != nil {
		return 0, 0, fmt.Errorf("parse upload size %q: %w", uploadStr, err)
	}

	downloadedBytes, err := parseSize(downloadStr)
	if err != nil {
		return 0, 0, fmt.Errorf("parse download size %q: %w", downloadStr, err)
	}

	return uploadedBytes, downloadedBytes, nil
}

// parseSize converts human-readable size strings like "282.82 Go", "6.33 Mo", "450 Ko" to bytes
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	// Match patterns like "282.82 Go", "1.5 Go", "500 Mo", "450 Ko", "1024 Go"
	re := regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*(Go|Mo|Ko|To|Bo)$`)
	matches := re.FindStringSubmatch(s)
	if matches == nil {
		return 0, fmt.Errorf("unrecognized format: %q", s)
	}

	value, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, fmt.Errorf("parse number: %w", err)
	}

	unit := strings.ToUpper(matches[2])
	var multiplier int64
	switch unit {
	case "TO":
		multiplier = 1000 * 1000 * 1000 * 1000
	case "GO":
		multiplier = 1000 * 1000 * 1000
	case "MO":
		multiplier = 1000 * 1000
	case "KO":
		multiplier = 1000
	case "BO":
		multiplier = 1
	default:
		return 0, fmt.Errorf("unknown unit: %s", matches[2])
	}

	return int64(value * float64(multiplier)), nil
}

// -----------------------------------------------------------------------------
// Prometheus
// -----------------------------------------------------------------------------

type exporterMetrics struct {
	totalUploaded   prometheus.Gauge
	totalDownloaded prometheus.Gauge
}

func newExporterMetrics(reg prometheus.Registerer) *exporterMetrics {
	m := &exporterMetrics{
		totalUploaded: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "yggreborn",
			Name:      "total_uploaded_bytes",
			Help:      "Total bytes uploaded by the user",
		}),
		totalDownloaded: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "yggreborn",
			Name:      "total_downloaded_bytes",
			Help:      "Total bytes downloaded by the user",
		}),
	}
	reg.MustRegister(m.totalUploaded, m.totalDownloaded)
	return m
}

func (m *exporterMetrics) update(uploaded, downloaded int64) {
	m.totalUploaded.Set(float64(uploaded))
	m.totalDownloaded.Set(float64(downloaded))
}

// -----------------------------------------------------------------------------
// Server
// -----------------------------------------------------------------------------

type server struct {
	mu             sync.RWMutex
	client         *YggRebornClient
	metrics        *exporterMetrics
	lastUploaded   int64
	lastDownloaded int64
	email          string
	password       string
	flareSolverURL string
	baseURL        string
}

func (s *server) metricsHandler(c *gin.Context) {
	c.Header("Content-Type", "text/plain; version=0.0.4")
	promhttp.Handler().ServeHTTP(c.Writer, c.Request)
}

// reconnect re-fetches cookies via FlareSolverr and re-login
func (s *server) reconnect() error {
	fmt.Println("[reconnect] Attempting reconnection...")
	flareCookies, ua, err := getFlareCookies(s.flareSolverURL, s.baseURL)
	if err != nil {
		return fmt.Errorf("reconnect cf_clearance: %w", err)
	}
	s.client.mu.Lock()
	s.client.flareCookies = flareCookies
	s.client.userAgent = ua
	s.client.sessionCookies = nil // reset session cookies on reconnect
	s.client.authenticated = false
	s.client.mu.Unlock()

	if err := s.client.Login(s.email, s.password); err != nil {
		return fmt.Errorf("reconnect login: %w", err)
	}
	fmt.Println("[reconnect] Reconnected successfully")
	return nil
}

func (s *server) scrapeAndRefresh() {
	uploaded, downloaded, err := s.client.FetchMetrics()
	if err != nil {
		fmt.Printf("[scraper] Fetch failed: %v — attempting reconnect...\n", err)

		if reconnectErr := s.reconnect(); reconnectErr != nil {
			fmt.Printf("[scraper] Reconnect failed: %v\n", reconnectErr)
			return
		}

		uploaded, downloaded, err = s.client.FetchMetrics()
		if err != nil {
			fmt.Printf("[scraper] Fetch still failing after reconnect: %v\n", err)
			return
		}
	}

	s.metrics.update(uploaded, downloaded)
	s.mu.Lock()
	s.lastUploaded = uploaded
	s.lastDownloaded = downloaded
	s.mu.Unlock()
	fmt.Printf("[scraper] ↑%d bytes ↓%d bytes\n", uploaded, downloaded)
}

func (s *server) startMetricsRefresher(interval time.Duration) {
	go s.scrapeAndRefresh()
	go func() {
		for range time.NewTicker(interval).C {
			s.scrapeAndRefresh()
		}
	}()
}

func (s *server) healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "healthy", "authenticated": s.client.IsAuthenticated()})
}

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	cfg := loadConfig()
	client := newYggRebornClient(cfg.BaseURL)
	metrics := newExporterMetrics(prometheus.DefaultRegisterer)
	srv := &server{
		client:         client,
		metrics:        metrics,
		email:          cfg.Email,
		password:       cfg.Password,
		flareSolverURL: cfg.FlareSolverURL,
		baseURL:        cfg.BaseURL,
	}

	if cfg.Email != "" && cfg.Password != "" {
		// Wait for FlareSolverr
		if err := waitForFlareSolverr(cfg.FlareSolverURL); err != nil {
			fmt.Printf("[flare] FlareSolverr not ready: %v (proceeding anyway)\n", err)
		}

		// Get cookies (cf_clearance + session) + User-Agent
		flareCookies, ua, err := getFlareCookies(cfg.FlareSolverURL, cfg.BaseURL)
		if err != nil {
			fmt.Printf("[flare] Failed to get cookies: %v\n", err)
		} else {
			client.mu.Lock()
			client.flareCookies = flareCookies
			client.userAgent = ua
			client.flareSolverURL = cfg.FlareSolverURL
			client.mu.Unlock()

			// Login immediately with fresh cookies
			fmt.Println("[auth] Attempting auto-login...")
			if err := client.Login(cfg.Email, cfg.Password); err != nil {
				fmt.Printf("[auth] Auto-login failed: %v\n", err)
			}
		}
	} else {
		fmt.Println("[auth] No credentials, skipping login")
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.GET(cfg.MetricsPath, srv.metricsHandler)
	r.GET("/health", srv.healthHandler)

	srv.startMetricsRefresher(cfg.ScrapeInterval)
	fmt.Printf("[server] Refresh interval: %v\n", cfg.ScrapeInterval)

	addr := ":" + cfg.Port
	fmt.Printf("[server] Listening on %s\n", addr)
	if err := r.Run(addr); err != nil {
		fmt.Printf("[server] Error: %v\n", err)
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func parseDuration(s string, fallback time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	for _, u := range []struct{ suffix string; mult time.Duration }{
		{"h", time.Hour}, {"m", time.Minute}, {"s", time.Second},
	} {
		if strings.HasSuffix(s, u.suffix) {
			var n int
			if _, err := fmt.Sscanf(strings.TrimSuffix(s, u.suffix), "%d", &n); err == nil && n > 0 {
				return time.Duration(n) * u.mult
			}
		}
	}
	return fallback
}

