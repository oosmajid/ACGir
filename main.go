package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const appName = "ACGir"

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address for the local web server")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser automatically")
	flag.Parse()

	manager := newJobManager()
	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/api/convert", manager.handleConvert)
	mux.HandleFunc("/api/jobs/", manager.handleJob)
	mux.HandleFunc("/api/download/", manager.handleDownload)

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot start server: %v", err)
	}

	localURL := "http://" + listener.Addr().String() + "/"
	log.Printf("%s is running at %s", appName, localURL)
	if !*noBrowser {
		go func() {
			time.Sleep(250 * time.Millisecond)
			if err := openBrowser(localURL); err != nil {
				log.Printf("open browser: %v", err)
			}
		}()
	}

	if err := http.Serve(listener, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped: %v", err)
	}
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

type jobManager struct {
	mu   sync.RWMutex
	jobs map[string]*job
}

func newJobManager() *jobManager {
	return &jobManager{jobs: make(map[string]*job)}
}

type convertRequest struct {
	URL    string `json:"url"`
	Cookie string `json:"cookie"`
}

type jobStatus struct {
	ID          string   `json:"id"`
	State       string   `json:"state"`
	Step        string   `json:"step"`
	Progress    float64  `json:"progress"`
	DownloadURL string   `json:"downloadUrl,omitempty"`
	Filename    string   `json:"filename,omitempty"`
	Error       string   `json:"error,omitempty"`
	Logs        []string `json:"logs"`
}

type job struct {
	mu         sync.Mutex
	status     jobStatus
	workDir    string
	outputPath string
	createdAt  time.Time
}

func (m *jobManager) handleConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req convertRequest
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "درخواست نامعتبر است."})
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	req.Cookie = cleanCookie(req.Cookie)

	parsed, err := url.Parse(req.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "لینک باید یک URL کامل http یا https باشد."})
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "فقط لینک‌های http و https پشتیبانی می‌شوند."})
		return
	}

	id, err := randomID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ساخت شناسه کار ممکن نشد."})
		return
	}
	workDir, err := makeJobDir(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ساخت پوشه موقت ممکن نشد."})
		return
	}

	j := &job{
		workDir:   workDir,
		createdAt: time.Now(),
		status: jobStatus{
			ID:       id,
			State:    "queued",
			Step:     "در صف اجرا",
			Progress: 0,
			Logs:     []string{},
		},
	}

	m.mu.Lock()
	m.jobs[id] = j
	m.mu.Unlock()

	go runConversion(j, req)
	writeJSON(w, http.StatusAccepted, j.snapshot())
}

func (m *jobManager) handleJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	j := m.get(id)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, j.snapshot())
}

func (m *jobManager) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/download/")
	j := m.get(id)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	status := j.snapshot()
	if status.State != "done" || j.outputPath == "" {
		http.Error(w, "not ready", http.StatusConflict)
		return
	}
	if _, err := os.Stat(j.outputPath); err != nil {
		http.NotFound(w, r)
		return
	}
	filename := status.Filename
	if filename == "" {
		filename = "acgir-audio.mp3"
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	http.ServeFile(w, r, j.outputPath)
}

func (m *jobManager) get(id string) *job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id]
}

func (j *job) snapshot() jobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.status
	s.Logs = append([]string(nil), j.status.Logs...)
	return s
}

func (j *job) update(state, step string, progress float64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if state != "" {
		j.status.State = state
	}
	if step != "" {
		j.status.Step = step
	}
	if progress >= 0 {
		if progress > 1 {
			progress = 1
		}
		j.status.Progress = progress
	}
}

func (j *job) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	j.mu.Lock()
	defer j.mu.Unlock()
	stamp := time.Now().Format("15:04:05")
	j.status.Logs = append(j.status.Logs, stamp+"  "+msg)
	if len(j.status.Logs) > 80 {
		j.status.Logs = j.status.Logs[len(j.status.Logs)-80:]
	}
}

func (j *job) finish(outputPath, filename string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.outputPath = outputPath
	j.status.State = "done"
	j.status.Step = "آماده دریافت"
	j.status.Progress = 1
	j.status.DownloadURL = "/api/download/" + j.status.ID
	j.status.Filename = filename
}

func (j *job) fail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.State = "error"
	j.status.Step = "خطا"
	j.status.Error = err.Error()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func makeJobDir(id string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "acgir", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func cleanCookie(cookie string) string {
	cookie = strings.TrimSpace(cookie)
	cookie = strings.TrimPrefix(cookie, "Cookie:")
	cookie = strings.TrimPrefix(cookie, "cookie:")
	return strings.TrimSpace(cookie)
}

func runConversion(j *job, req convertRequest) {
	defer func() {
		if p := recover(); p != nil {
			j.fail(fmt.Errorf("خطای داخلی: %v", p))
		}
	}()

	client := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 45 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}

	j.update("running", "تحلیل لینک Adobe Connect", 0.03)
	j.logf("شروع با لینک: %s", req.URL)
	discovery := discover(req.URL, client, req.Cookie, j)

	var mediaFiles []string
	zipPath := ""
	var zipErr error
	if len(discovery.ZipCandidates) > 0 {
		j.update("running", "جست‌وجوی آرشیو خروجی", 0.10)
		var source string
		var err error
		zipPath, source, err = downloadFirstZip(client, discovery.ZipCandidates, req.Cookie, j.workDir, j)
		if err == nil {
			j.logf("آرشیو پیدا شد: %s", source)
		} else {
			zipErr = err
			j.logf("آرشیو zip مستقیم پیدا نشد؛ مسیر XML را امتحان می‌کنم.")
		}
	}

	if zipPath == "" {
		if len(discovery.RecordingBases) == 0 {
			if discovery.LoginURL != "" {
				j.fail(fmt.Errorf("لینک به صفحه ورود برگشت: %s. برنامه بدون Cookie نشست نمی‌تواند از Moodle/Adobe Connect عبور کند. در مرورگر وارد سامانه شوید و Cookie همان نشست را در بخش Cookie وارد کنید، یا لینک مستقیم ضبط/zip را بدهید.", discovery.LoginURL))
				return
			}
			if zipErr != nil {
				j.fail(fmt.Errorf("دانلود آرشیو مستقیم موفق نبود و مسیر واقعی ضبط هم از لینک پیدا نشد. آخرین خطا: %v", zipErr))
				return
			}
		}
		j.update("running", "جست‌وجوی فایل‌های صوتی از XML", 0.22)
		var err error
		mediaFiles, err = downloadDirectMedia(client, discovery.RecordingBases, req.Cookie, j.workDir, j)
		if err != nil {
			j.fail(err)
			return
		}
		j.logf("%d فایل رسانه‌ای از مسیر XML دریافت شد.", len(mediaFiles))
	}

	j.update("running", "استخراج صدای MP3", 0.68)
	output := filepath.Join(j.workDir, "audio.mp3")
	var result extractResult
	var err error
	if zipPath != "" {
		if ffmpegPath, ok := findFFmpeg(); ok {
			result, err = convertLargestZipMediaWithFFmpeg(zipPath, j.workDir, output, ffmpegPath, j)
			if err == nil {
				filename := safeOutputName(req.URL)
				j.logf("خروجی ساخته شد: %s (%s)", filename, humanBytes(result.Bytes))
				j.logf("منبع‌های استفاده‌شده: %s", strings.Join(result.SourceNames, "، "))
				j.finish(output, filename)
				return
			}
			j.logf("تبدیل با FFmpeg موفق نبود؛ استخراج بدون dependency را امتحان می‌کنم: %v", err)
		}
		result, err = extractFromZip(zipPath, j.workDir, output, j)
	} else {
		if ffmpegPath, ok := findFFmpeg(); ok {
			result, err = convertLargestFileWithFFmpeg(mediaFiles, j.workDir, output, ffmpegPath, j)
			if err == nil {
				filename := safeOutputName(req.URL)
				j.logf("خروجی ساخته شد: %s (%s)", filename, humanBytes(result.Bytes))
				j.logf("منبع‌های استفاده‌شده: %s", strings.Join(result.SourceNames, "، "))
				j.finish(output, filename)
				return
			}
			j.logf("تبدیل با FFmpeg موفق نبود؛ استخراج بدون dependency را امتحان می‌کنم: %v", err)
		}
		result, err = extractFromFiles(mediaFiles, j.workDir, output, j)
	}
	if err != nil {
		j.fail(err)
		return
	}

	filename := safeOutputName(req.URL)
	j.logf("خروجی ساخته شد: %s (%s)", filename, humanBytes(result.Bytes))
	j.logf("منبع‌های استفاده‌شده: %s", strings.Join(result.SourceNames, "، "))
	j.finish(output, filename)
}

type discoveryInfo struct {
	ZipCandidates  []string
	RecordingBases []string
	LoginURL       string
}

func discover(raw string, client *http.Client, cookie string, j *job) discoveryInfo {
	info := discoveryInfo{}
	seenZip := map[string]bool{}
	seenBase := map[string]bool{}

	addZip := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seenZip[candidate] {
			return
		}
		seenZip[candidate] = true
		info.ZipCandidates = append(info.ZipCandidates, candidate)
	}
	addBase := func(base string) {
		base = strings.TrimSpace(base)
		if base == "" || seenBase[base] {
			return
		}
		seenBase[base] = true
		info.RecordingBases = append(info.RecordingBases, base)
		for _, c := range zipCandidatesForRecordingBase(base) {
			addZip(c)
		}
	}

	if isLikelyZipURL(raw) {
		addZip(ensureDownloadZip(raw))
	}
	for _, c := range zipCandidatesForLaunchURL(raw) {
		addZip(c)
	}
	if !isConnectorURL(raw) && !isLoginURL(raw) && looksLikeRecordingURL(raw) {
		if base := recordingBaseFromURL(raw); base != "" {
			addBase(base)
		}
	}

	if finalURL, body, err := fetchLandingPage(client, raw, cookie, 4<<20); err == nil {
		if finalURL != "" && finalURL != raw {
			j.logf("لینک واسط به مسیر دیگری رسید: %s", finalURL)
			if isLoginURL(finalURL) {
				info.LoginURL = finalURL
				j.logf("لینک به صفحه ورود برگشت؛ مسیر ضبط واقعی بدون نشست قابل مشاهده نیست.")
			}
			if isLikelyZipURL(finalURL) {
				addZip(ensureDownloadZip(finalURL))
			}
			if !isConnectorURL(finalURL) && !isLoginURL(finalURL) && looksLikeRecordingURL(finalURL) {
				if base := recordingBaseFromURL(finalURL); base != "" {
					addBase(base)
				}
			}
		}
		for _, found := range parseLandingURLs(finalURLOrRaw(finalURL, raw), body) {
			if isLikelyZipURL(found) {
				addZip(ensureDownloadZip(found))
			}
			if !isConnectorURL(found) && !isLoginURL(found) && looksLikeRecordingURL(found) {
				if base := recordingBaseFromURL(found); base != "" {
					addBase(base)
				}
			}
		}
	} else {
		j.logf("خواندن لینک اولیه برای کشف redirect/HTML موفق نبود: %v", err)
	}

	if !isConnectorURL(raw) && !isLoginURL(raw) {
		modeXML := withQueryParam(raw, "mode", "xml")
		if body, err := getSmall(client, modeXML, cookie, 2<<20); err == nil {
			j.logf("پاسخ mode=xml دریافت شد.")
			for _, p := range parseURLPaths(body) {
				if base := recordingBaseFromURLPath(raw, p); base != "" {
					addBase(base)
				}
			}
			for _, scoID := range parseSCOIDs(body) {
				apiURL := apiXMLURL(raw, scoID)
				if apiURL == "" {
					continue
				}
				if scoBody, err := getSmall(client, apiURL, cookie, 2<<20); err == nil {
					for _, p := range parseURLPaths(scoBody) {
						if base := recordingBaseFromURLPath(raw, p); base != "" {
							addBase(base)
						}
					}
				}
			}
		} else {
			j.logf("mode=xml قابل دریافت نبود: %v", err)
		}
	}

	return info
}

func zipCandidatesForLaunchURL(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	u.Fragment = ""
	p := strings.TrimRight(u.Path, "/")
	if p == "" {
		return nil
	}
	u.Path = p + "/output/lecture.zip"
	q := u.Query()
	q.Set("download", "zip")
	q.Del("mode")
	u.RawQuery = q.Encode()
	out := []string{u.String()}

	sessionOnly := *u
	q2 := preservedRecordingQuery(&sessionOnly)
	q2.Set("download", "zip")
	sessionOnly.RawQuery = q2.Encode()
	if sessionOnly.String() != out[0] {
		out = append(out, sessionOnly.String())
	}
	return out
}

func finalURLOrRaw(finalURL, raw string) string {
	if finalURL != "" {
		return finalURL
	}
	return raw
}

func fetchLandingPage(client *http.Client, raw, cookie string, limit int64) (string, string, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return "", "", err
	}
	addRequestHeaders(req, cookie)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Request.URL.String(), "", fmt.Errorf("HTTP %s", resp.Status)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(resp.Body, limit)); err != nil {
		return resp.Request.URL.String(), "", err
	}
	return resp.Request.URL.String(), buf.String(), nil
}

var (
	htmlURLAttrRE   = regexp.MustCompile(`(?is)\b(?:href|src|action|data|url)\s*=\s*["']([^"']+)["']`)
	htmlQuotedURLRE = regexp.MustCompile(`(?is)["']((?:https?://|/)[^"'<>\s]+)["']`)
	metaRefreshRE   = regexp.MustCompile(`(?is)\burl\s*=\s*([^"'<>\s]+)`)
)

func parseLandingURLs(baseRaw, body string) []string {
	base, err := url.Parse(baseRaw)
	if err != nil || base.Scheme == "" || base.Host == "" || body == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(ref string) {
		ref = strings.TrimSpace(html.UnescapeString(ref))
		ref = strings.Trim(ref, `"'`)
		if ref == "" || strings.HasPrefix(strings.ToLower(ref), "javascript:") || strings.HasPrefix(strings.ToLower(ref), "mailto:") {
			return
		}
		u, err := url.Parse(ref)
		if err != nil {
			return
		}
		resolved := base.ResolveReference(u)
		resolved.Fragment = ""
		s := resolved.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, re := range []*regexp.Regexp{htmlURLAttrRE, htmlQuotedURLRE, metaRefreshRE} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			add(m[1])
		}
	}
	return out
}

func looksLikeRecordingURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	lower := strings.ToLower(u.Path)
	if isLoginURL(raw) || strings.Contains(lower, "/lib/") || strings.Contains(lower, "/theme/") || strings.Contains(lower, "/pluginfile.php") || strings.Contains(lower, "/javascript.php") {
		return false
	}
	if strings.Contains(lower, "/output/") {
		return true
	}
	if strings.Contains(lower, "/mod/adobeconnect/") || strings.Contains(lower, "joinrecording.php") {
		return false
	}
	last := strings.ToLower(strings.Trim(path.Base(strings.TrimRight(u.Path, "/")), "/"))
	return isAdobeConnectPPath(last) || strings.Contains(lower, "/recording") || strings.Contains(lower, "/meeting")
}

func isAdobeConnectPPath(last string) bool {
	if len(last) < 2 || last[0] != 'p' {
		return false
	}
	for _, r := range last[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isConnectorURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	lower := strings.ToLower(u.Path)
	return strings.Contains(lower, "/mod/adobeconnect/") || strings.Contains(lower, "joinrecording.php")
}

func isLoginURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	lower := strings.ToLower(u.Path)
	return strings.Contains(lower, "/login/") || strings.HasSuffix(lower, "/login/index.php")
}

func isLikelyZipURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".zip") || strings.Contains(strings.ToLower(u.RawQuery), "download=zip")
}

func ensureDownloadZip(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("download") == "" {
		q.Set("download", "zip")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func recordingBaseFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.RawQuery = preservedRecordingQuery(u).Encode()
	u.Fragment = ""
	p := u.Path
	if idx := strings.Index(strings.ToLower(p), "/output/"); idx >= 0 {
		p = p[:idx]
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	u.Path = p + "/"
	return u.String()
}

func recordingBaseFromURLPath(serverRaw, urlPath string) string {
	server, err := url.Parse(serverRaw)
	if err != nil || server.Scheme == "" || server.Host == "" {
		return ""
	}
	urlPath = strings.TrimSpace(html.UnescapeString(urlPath))
	if urlPath == "" {
		return ""
	}
	if strings.HasPrefix(urlPath, "http://") || strings.HasPrefix(urlPath, "https://") {
		return recordingBaseFromURL(urlPath)
	}
	server.RawQuery = preservedRecordingQuery(server).Encode()
	server.Fragment = ""
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	server.Path = strings.TrimRight(urlPath, "/") + "/"
	return server.String()
}

func zipCandidatesForRecordingBase(baseRaw string) []string {
	u, err := url.Parse(baseRaw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	basePath := strings.TrimRight(u.Path, "/")
	if basePath == "" {
		return nil
	}
	last := path.Base(basePath)
	last = strings.TrimSuffix(last, ".zip")
	names := []string{"lecture", last}
	if strings.ToLower(last) != "recording" {
		names = append(names, "recording")
	}
	if strings.ToLower(last) != "archive" {
		names = append(names, "archive")
	}
	seenName := map[string]bool{}
	var out []string
	for _, name := range names {
		if name == "" || name == "." || name == "/" {
			continue
		}
		lowerName := strings.ToLower(name)
		if seenName[lowerName] {
			continue
		}
		seenName[lowerName] = true
		c := *u
		c.Path = basePath + "/output/" + name + ".zip"
		q := preservedRecordingQuery(&c)
		q.Set("download", "zip")
		c.RawQuery = q.Encode()
		out = append(out, c.String())
	}
	return out
}

func preservedRecordingQuery(u *url.URL) url.Values {
	out := url.Values{}
	if u == nil {
		return out
	}
	q := u.Query()
	for _, key := range []string{"session"} {
		for _, value := range q[key] {
			if value != "" {
				out.Add(key, value)
			}
		}
	}
	return out
}

func withQueryParam(raw, key, value string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

func apiXMLURL(serverRaw, scoID string) string {
	u, err := url.Parse(serverRaw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.Path = "/api/xml"
	q := url.Values{}
	q.Set("action", "sco-info")
	q.Set("sco-id", scoID)
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

var (
	urlPathElementRE = regexp.MustCompile(`(?is)<url-path>\s*([^<]+?)\s*</url-path>`)
	urlPathAttrRE    = regexp.MustCompile(`(?is)\burl-path\s*=\s*["']([^"']+)["']`)
	scoIDElementRE   = regexp.MustCompile(`(?is)<sco-id>\s*(\d+)\s*</sco-id>`)
	scoIDAttrRE      = regexp.MustCompile(`(?is)\bsco-id\s*=\s*["']?(\d+)["']?`)
)

func parseURLPaths(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range []*regexp.Regexp{urlPathElementRE, urlPathAttrRE} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			p := strings.TrimSpace(html.UnescapeString(m[1]))
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func parseSCOIDs(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range []*regexp.Regexp{scoIDElementRE, scoIDAttrRE} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			id := strings.TrimSpace(m[1])
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func getSmall(client *http.Client, raw, cookie string, limit int64) (string, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	addRequestHeaders(req, cookie)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(resp.Body, limit)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func addRequestHeaders(req *http.Request, cookie string) {
	req.Header.Set("User-Agent", "ACGir/1.0 (+local Adobe Connect audio extractor)")
	req.Header.Set("Accept", "*/*")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

func downloadFirstZip(client *http.Client, candidates []string, cookie, workDir string, j *job) (string, string, error) {
	var errs []string
	for i, candidate := range candidates {
		j.update("running", fmt.Sprintf("امتحان آرشیو %d از %d", i+1, len(candidates)), 0.12+0.25*float64(i)/float64(max(1, len(candidates))))
		dest := filepath.Join(workDir, fmt.Sprintf("recording-%02d.zip", i+1))
		ok, err := tryDownloadZip(client, candidate, cookie, dest, j)
		if ok {
			return dest, candidate, nil
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", candidate, err))
			j.logf("مسیر zip ناموفق بود: %s (%v)", candidate, err)
		}
		_ = os.Remove(dest)
	}
	if len(errs) == 0 {
		return "", "", errors.New("هیچ مسیر zip ساخته نشد.")
	}
	return "", "", errors.New(strings.Join(errs, "\n"))
}

func tryDownloadZip(client *http.Client, raw, cookie, dest string, j *job) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return false, err
	}
	addRequestHeaders(req, cookie)
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := readSnippet(resp.Body, 300)
		if snippet != "" {
			return false, fmt.Errorf("HTTP %s: %s", resp.Status, snippet)
		}
		return false, fmt.Errorf("HTTP %s", resp.Status)
	}

	header := make([]byte, 4)
	n, readErr := io.ReadFull(resp.Body, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return false, readErr
	}
	if n < 2 || string(header[:2]) != "PK" {
		more := readSnippet(resp.Body, 500)
		snippet := strings.TrimSpace(string(header[:n]) + more)
		if snippet == "" {
			snippet = "پاسخ zip نبود."
		}
		return false, fmt.Errorf("%s", clip(snippet, 180))
	}

	out, err := os.Create(dest)
	if err != nil {
		return false, err
	}
	defer out.Close()
	if _, err := out.Write(header[:n]); err != nil {
		return false, err
	}

	total := resp.ContentLength
	written := int64(n)
	buf := make([]byte, 128*1024)
	lastUpdate := time.Now()
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := out.Write(buf[:nr])
			if ew != nil {
				return false, ew
			}
			if nw != nr {
				return false, io.ErrShortWrite
			}
			written += int64(nw)
			if total > 0 && time.Since(lastUpdate) > 300*time.Millisecond {
				p := 0.20 + 0.35*(float64(written)/float64(total))
				j.update("running", fmt.Sprintf("دریافت آرشیو (%s از %s)", humanBytes(written), humanBytes(total)), p)
				lastUpdate = time.Now()
			}
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				break
			}
			return false, er
		}
	}
	return true, nil
}

func readSnippet(r io.Reader, limit int64) string {
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, io.LimitReader(r, limit))
	return strings.TrimSpace(buf.String())
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var (
	mediaRefRE        = regexp.MustCompile(`(?i)(?:https?://[^\s"'<>]+|[A-Za-z0-9._~%+/\-]+)\.(?:flv|mp3|m4a|aac)`)
	mediaSidecarXMLRE = regexp.MustCompile(`(?i)(?:https?://[^\s"'<>]+|[A-Za-z0-9._~%+/\-]*)(?:cameraVoip|telephony|audio|voip|mainstream|indexstream)[A-Za-z0-9._~%+/\-]*\.xml`)
	streamNameRE      = regexp.MustCompile(`(?i)\b(?:cameraVoip|telephony|audio|voip|mainstream|indexstream)(?:[_-]\d+){0,3}\b`)
)

func downloadDirectMedia(client *http.Client, bases []string, cookie, workDir string, j *job) ([]string, error) {
	if len(bases) == 0 {
		return nil, errors.New("مسیر ضبط پیدا نشد. اگر لینک خصوصی است، کوکی نشست Adobe Connect را وارد کنید.")
	}
	mediaDir := filepath.Join(workDir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, err
	}

	seenMedia := map[string]bool{}
	var mediaURLs []string
	xmlRead := 0
	refsFound := 0
	addMediaURL := func(base *url.URL, ref string) {
		mediaURL := resolveRelative(base, ref)
		if mediaURL != "" && !seenMedia[mediaURL] {
			seenMedia[mediaURL] = true
			mediaURLs = append(mediaURLs, mediaURL)
		}
	}
	for _, base := range bases {
		outputBase, err := outputBaseURL(base)
		if err != nil {
			continue
		}
		for _, xmlName := range []string{"indexstream.xml", "mainstream.xml", "cameraVoip.xml", "telephony-files.xml", "telephony.xml", "sco_metadata.xml", "edit.xml"} {
			xmlURL := resolveRelative(outputBase, xmlName)
			body, err := getSmall(client, xmlURL, cookie, 8<<20)
			if err != nil {
				continue
			}
			xmlRead++
			j.logf("XML خوانده شد: %s", xmlName)
			for _, ref := range parseMediaRefs(body) {
				refsFound++
				addMediaURL(outputBase, ref)
			}
		}
	}

	if len(mediaURLs) == 0 {
		for _, base := range bases {
			outputBase, err := outputBaseURL(base)
			if err != nil {
				continue
			}
			j.logf("اسم فایل صوتی در XML مستقیم نبود؛ مسیرهای رایج cameraVoip را امتحان می‌کنم.")
			for _, ref := range commonAudioCandidates() {
				addMediaURL(outputBase, ref)
			}
		}
	}

	sort.SliceStable(mediaURLs, func(i, k int) bool {
		return mediaScore(mediaURLs[i]) > mediaScore(mediaURLs[k])
	})
	if len(mediaURLs) > 80 {
		mediaURLs = mediaURLs[:80]
	}
	if len(mediaURLs) == 0 {
		if xmlRead > 0 && refsFound == 0 {
			return nil, errors.New("XMLهای ضبط خوانده شدند، اما اسم فایل صوتی قابل تشخیص نبود. ممکن است نام‌گذاری این سرور با الگوهای Adobe Connect فرق داشته باشد.")
		}
		return nil, errors.New("در XML ضبط هیچ فایل flv/mp3 پیدا نشد. ممکن است ضبط عمومی نباشد یا سرور دانلود خروجی را بسته باشد.")
	}

	var files []string
	for i, mediaURL := range mediaURLs {
		j.update("running", fmt.Sprintf("دریافت رسانه %d از %d", i+1, len(mediaURLs)), 0.35+0.25*float64(i)/float64(max(1, len(mediaURLs))))
		local := filepath.Join(mediaDir, fmt.Sprintf("%03d-%s", i+1, sanitizeFileName(path.Base(mustPath(mediaURL)))))
		if err := downloadMediaFile(client, mediaURL, cookie, local); err != nil {
			j.logf("دریافت رسانه ناموفق بود: %s (%v)", mediaURL, err)
			continue
		}
		files = append(files, local)
	}
	if len(files) == 0 {
		return nil, errors.New("هیچ فایل رسانه‌ای قابل دریافت نبود. اگر لینک نیاز به ورود دارد، کوکی نشست را وارد کنید.")
	}
	return files, nil
}

func outputBaseURL(baseRaw string) (*url.URL, error) {
	u, err := url.Parse(baseRaw)
	if err != nil {
		return nil, err
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/") + "/output/"
	return u, nil
}

func resolveRelative(base *url.URL, ref string) string {
	ref = strings.TrimSpace(html.UnescapeString(ref))
	ref = strings.Trim(ref, `"'`)
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return base.ResolveReference(u).String()
}

func parseMediaRefs(body string) []string {
	seen := map[string]bool{}
	var refs []string
	add := func(ref string) {
		ref = strings.TrimRight(html.UnescapeString(ref), ".,;)")
		ref = strings.Trim(ref, `"'`)
		if ref != "" && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	for _, m := range mediaRefRE.FindAllString(body, -1) {
		add(m)
	}
	for _, m := range mediaSidecarXMLRE.FindAllString(body, -1) {
		add(replaceExt(m, ".flv"))
	}
	for _, m := range streamNameRE.FindAllString(body, -1) {
		if strings.ContainsAny(m, "_-0123456789") {
			add(m + ".flv")
			continue
		}
		for _, ref := range commonCandidatesForStream(m) {
			add(ref)
		}
	}
	return refs
}

func replaceExt(name, ext string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		return name[:idx] + ext
	}
	return name + ext
}

func commonAudioCandidates() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}

	for _, name := range []string{
		"cameraVoip.flv",
		"cameraVoip_0.flv",
		"cameraVoip_1.flv",
		"telephony.flv",
		"audio.flv",
		"voip.flv",
		"mainstream.flv",
		"indexstream.flv",
	} {
		add(name)
	}
	for first := 0; first <= 4; first++ {
		for second := 0; second <= 40; second++ {
			add(fmt.Sprintf("cameraVoip_%d_%d.flv", first, second))
		}
	}
	return out
}

func commonCandidatesForStream(stream string) []string {
	stream = strings.ToLower(stream)
	switch {
	case strings.Contains(stream, "cameravoip"), strings.Contains(stream, "voip"):
		return commonCameraVoipCandidates()
	case strings.Contains(stream, "telephony"):
		return []string{"telephony.flv", "telephony_0.flv", "telephony_1.flv"}
	case strings.Contains(stream, "audio"):
		return []string{"audio.flv", "audio_0.flv", "audio_1.flv"}
	case strings.Contains(stream, "mainstream"):
		return []string{"mainstream.flv"}
	case strings.Contains(stream, "indexstream"):
		return []string{"indexstream.flv"}
	default:
		return nil
	}
}

func commonCameraVoipCandidates() []string {
	var out []string
	out = append(out, "cameraVoip.flv", "cameraVoip_0.flv", "cameraVoip_1.flv")
	for first := 0; first <= 4; first++ {
		for second := 0; second <= 40; second++ {
			out = append(out, fmt.Sprintf("cameraVoip_%d_%d.flv", first, second))
		}
	}
	return out
}

func mustPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Path
}

func downloadFile(client *http.Client, raw, cookie, dest string) error {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	addRequestHeaders(req, cookie)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func downloadMediaFile(client *http.Client, raw, cookie, dest string) error {
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	addRequestHeaders(req, cookie)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	header := make([]byte, 4)
	n, readErr := io.ReadFull(resp.Body, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return readErr
	}
	if !looksLikeSupportedMedia(raw, header[:n]) {
		return errors.New("پاسخ سرور فایل رسانه‌ای قابل پردازش نبود")
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if n > 0 {
		if _, err := out.Write(header[:n]); err != nil {
			return err
		}
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

func looksLikeSupportedMedia(raw string, header []byte) bool {
	ext := strings.ToLower(path.Ext(mustPath(raw)))
	switch ext {
	case ".flv":
		return len(header) >= 3 && string(header[:3]) == "FLV"
	case ".mp3":
		return isMP3Header(header)
	case ".m4a", ".aac":
		return len(header) > 0
	default:
		return false
	}
}

func isMP3Header(header []byte) bool {
	if len(header) >= 3 && string(header[:3]) == "ID3" {
		return true
	}
	return len(header) >= 2 && header[0] == 0xff && (header[1]&0xe0) == 0xe0
}

type mediaSource struct {
	Name string
	Size int64
	Open func() (io.ReadCloser, error)
}

func findFFmpeg() (string, bool) {
	if env := strings.TrimSpace(os.Getenv("FFMPEG_BINARY")); env != "" {
		if isExecutableFile(env) {
			return env, true
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		names := []string{"ffmpeg"}
		if runtime.GOOS == "windows" {
			names = []string{"ffmpeg.exe", "ffmpeg"}
		}
		for _, name := range names {
			candidate := filepath.Join(dir, name)
			if isExecutableFile(candidate) {
				return candidate, true
			}
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil && p != "" {
		return p, true
	}
	return "", false
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

func convertLargestZipMediaWithFFmpeg(zipPath, workDir, output, ffmpegPath string, j *job) (extractResult, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return extractResult{}, err
	}
	defer zr.Close()

	var selected *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isFFmpegMediaName(f.Name) {
			continue
		}
		if selected == nil || f.UncompressedSize64 > selected.UncompressedSize64 {
			selected = f
		}
	}
	if selected == nil {
		return extractResult{}, errors.New("هیچ فایل flv/mp4 قابل تبدیل با FFmpeg در zip پیدا نشد")
	}

	input := filepath.Join(workDir, "ffmpeg-input-"+sanitizeFileName(path.Base(selected.Name)))
	if input == filepath.Join(workDir, "ffmpeg-input-") {
		input = filepath.Join(workDir, "ffmpeg-input-media")
	}
	n, err := copyZipFileToPath(selected, input)
	if err != nil {
		return extractResult{}, err
	}
	j.logf("فایل اصلی برای تبدیل انتخاب شد: %s (%s)", selected.Name, humanBytes(n))
	return convertMediaWithFFmpeg(input, selected.Name, output, ffmpegPath, j)
}

func convertLargestFileWithFFmpeg(paths []string, workDir, output, ffmpegPath string, j *job) (extractResult, error) {
	var selected string
	var selectedSize int64
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || !isFFmpegMediaName(p) {
			continue
		}
		if selected == "" || info.Size() > selectedSize {
			selected = p
			selectedSize = info.Size()
		}
	}
	if selected == "" {
		return extractResult{}, errors.New("هیچ فایل flv/mp4 قابل تبدیل با FFmpeg پیدا نشد")
	}
	_ = workDir
	j.logf("فایل اصلی برای تبدیل انتخاب شد: %s (%s)", filepath.Base(selected), humanBytes(selectedSize))
	return convertMediaWithFFmpeg(selected, filepath.Base(selected), output, ffmpegPath, j)
}

func isFFmpegMediaName(name string) bool {
	ext := strings.ToLower(path.Ext(strings.ToLower(name)))
	switch ext {
	case ".flv", ".mp4", ".m4a", ".aac", ".mp3":
		return true
	default:
		return false
	}
}

func copyZipFileToPath(f *zip.File, dest string) (int64, error) {
	in, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, in)
}

func convertMediaWithFFmpeg(input, sourceName, output, ffmpegPath string, j *job) (extractResult, error) {
	j.update("running", "تبدیل با FFmpeg", 0.74)
	cmd := exec.Command(ffmpegPath, "-y", "-i", input, "-vn", "-acodec", "libmp3lame", "-q:a", "2", output)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return extractResult{}, fmt.Errorf("FFmpeg: %s", clip(msg, 600))
		}
		return extractResult{}, err
	}
	info, err := os.Stat(output)
	if err != nil {
		return extractResult{}, err
	}
	if info.Size() == 0 {
		return extractResult{}, errors.New("FFmpeg خروجی خالی ساخت")
	}
	return extractResult{Bytes: info.Size(), SourceNames: []string{sourceName}}, nil
}

type extractedCandidate struct {
	Name  string
	Path  string
	Kind  string
	Size  int64
	Score int
}

type extractResult struct {
	Bytes       int64
	SourceNames []string
}

func extractFromZip(zipPath, workDir, output string, j *job) (extractResult, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return extractResult{}, fmt.Errorf("باز کردن zip ممکن نشد: %w", err)
	}
	defer zr.Close()

	var sources []mediaSource
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		f := f
		sources = append(sources, mediaSource{
			Name: f.Name,
			Size: int64(f.UncompressedSize64),
			Open: f.Open,
		})
	}
	return extractFromSources(sources, workDir, output, j)
}

func extractFromFiles(paths []string, workDir, output string, j *job) (extractResult, error) {
	var sources []mediaSource
	for _, p := range paths {
		p := p
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		sources = append(sources, mediaSource{
			Name: filepath.Base(p),
			Size: st.Size(),
			Open: func() (io.ReadCloser, error) {
				return os.Open(p)
			},
		})
	}
	return extractFromSources(sources, workDir, output, j)
}

func extractFromSources(sources []mediaSource, workDir, output string, j *job) (extractResult, error) {
	sort.SliceStable(sources, func(i, k int) bool {
		return strings.ToLower(sources[i].Name) < strings.ToLower(sources[k].Name)
	})

	extractDir := filepath.Join(workDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return extractResult{}, err
	}

	var candidates []extractedCandidate
	var unsupported []string
	mediaIndex := 0
	for i, src := range sources {
		lower := strings.ToLower(src.Name)
		ext := strings.ToLower(path.Ext(lower))
		if ext != ".mp3" && ext != ".flv" {
			continue
		}
		j.update("running", fmt.Sprintf("بررسی رسانه %d از %d", i+1, len(sources)), 0.68+0.18*float64(i)/float64(max(1, len(sources))))
		switch ext {
		case ".mp3":
			mediaIndex++
			temp := filepath.Join(extractDir, fmt.Sprintf("%03d.mp3", mediaIndex))
			n, err := copySourceToFile(src, temp)
			if err != nil {
				j.logf("کپی MP3 ناموفق بود: %s (%v)", src.Name, err)
				continue
			}
			candidates = append(candidates, extractedCandidate{
				Name:  src.Name,
				Path:  temp,
				Kind:  "mp3",
				Size:  n,
				Score: mediaScore(src.Name) + 20,
			})
			j.logf("MP3 پیدا شد: %s (%s)", src.Name, humanBytes(n))
		case ".flv":
			mediaIndex++
			temp := filepath.Join(extractDir, fmt.Sprintf("%03d.mp3", mediaIndex))
			info, err := extractMP3FromFLVSource(src, temp)
			if err != nil {
				j.logf("خواندن FLV ناموفق بود: %s (%v)", src.Name, err)
				_ = os.Remove(temp)
				continue
			}
			if info.MP3Bytes > 0 {
				candidates = append(candidates, extractedCandidate{
					Name:  src.Name,
					Path:  temp,
					Kind:  "flv-mp3",
					Size:  info.MP3Bytes,
					Score: mediaScore(src.Name),
				})
				j.logf("صدای MP3 از FLV استخراج شد: %s (%s)", src.Name, humanBytes(info.MP3Bytes))
				continue
			}
			_ = os.Remove(temp)
			if len(info.Codecs) > 0 || info.AudioTags > 0 {
				unsupported = append(unsupported, fmt.Sprintf("%s [%s]", src.Name, strings.Join(info.Codecs, ",")))
			}
		}
	}

	if len(candidates) == 0 {
		if len(unsupported) > 0 {
			return extractResult{}, fmt.Errorf("فایل صوتی پیدا شد، اما کدک آن MP3 نبود: %s. این نسخه بدون dependency فقط MP3 داخل FLV/MP3 مستقیم را استخراج می‌کند؛ برای AAC/Speex/Nellymoser باید خروجی MP4/FLV رسمی Adobe Connect یا FFmpeg استفاده شود.", strings.Join(unsupported, "، "))
		}
		return extractResult{}, errors.New("هیچ فایل صوتی MP3 در خروجی Adobe Connect پیدا نشد.")
	}

	selected := selectCandidates(candidates)
	j.update("running", "ساخت فایل نهایی", 0.92)
	n, err := concatenateMP3(selected, output)
	if err != nil {
		return extractResult{}, err
	}
	names := make([]string, 0, len(selected))
	for _, c := range selected {
		names = append(names, c.Name)
	}
	return extractResult{Bytes: n, SourceNames: names}, nil
}

func copySourceToFile(src mediaSource, dest string) (int64, error) {
	in, err := src.Open()
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, in)
}

func selectCandidates(candidates []extractedCandidate) []extractedCandidate {
	sort.SliceStable(candidates, func(i, k int) bool {
		if candidates[i].Score != candidates[k].Score {
			return candidates[i].Score > candidates[k].Score
		}
		if candidates[i].Size != candidates[k].Size {
			return candidates[i].Size > candidates[k].Size
		}
		return strings.ToLower(candidates[i].Name) < strings.ToLower(candidates[k].Name)
	})
	maxScore := candidates[0].Score
	var selected []extractedCandidate
	for _, c := range candidates {
		if c.Score == maxScore {
			selected = append(selected, c)
		}
	}
	sort.SliceStable(selected, func(i, k int) bool {
		return naturalLess(selected[i].Name, selected[k].Name)
	})
	return selected
}

func mediaScore(name string) int {
	lower := strings.ToLower(name)
	score := 10
	switch {
	case strings.Contains(lower, "telephony"):
		score += 90
	case strings.Contains(lower, "cameravoip"), strings.Contains(lower, "camera_voip"):
		score += 85
	case strings.Contains(lower, "camera") && strings.Contains(lower, "voip"):
		score += 80
	case strings.Contains(lower, "voip"):
		score += 70
	case strings.Contains(lower, "audio"):
		score += 60
	case strings.Contains(lower, "screenshare"):
		score += 5
	}
	if strings.HasSuffix(lower, ".mp3") {
		score += 20
	}
	return score
}

func naturalLess(a, b string) bool {
	as := splitNatural(strings.ToLower(a))
	bs := splitNatural(strings.ToLower(b))
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, aErr := strconv.Atoi(as[i])
		bi, bErr := strconv.Atoi(bs[i])
		if aErr == nil && bErr == nil {
			if ai != bi {
				return ai < bi
			}
			continue
		}
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

func splitNatural(s string) []string {
	var parts []string
	var buf strings.Builder
	kindDigit := false
	hasKind := false
	for _, r := range s {
		digit := r >= '0' && r <= '9'
		if hasKind && digit != kindDigit {
			parts = append(parts, buf.String())
			buf.Reset()
		}
		kindDigit = digit
		hasKind = true
		buf.WriteRune(r)
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}
	return parts
}

func concatenateMP3(candidates []extractedCandidate, output string) (int64, error) {
	out, err := os.Create(output)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	var total int64
	for i, c := range candidates {
		in, err := os.Open(c.Path)
		if err != nil {
			return 0, err
		}
		n, err := copyMP3Payload(out, in, i > 0)
		closeErr := in.Close()
		if err != nil {
			return 0, err
		}
		if closeErr != nil {
			return 0, closeErr
		}
		total += n
	}
	return total, nil
}

func copyMP3Payload(dst io.Writer, src *os.File, stripID3 bool) (int64, error) {
	if !stripID3 {
		return io.Copy(dst, src)
	}
	header := make([]byte, 10)
	n, err := io.ReadFull(src, header)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			_, _ = src.Seek(0, io.SeekStart)
			return io.Copy(dst, src)
		}
		return 0, err
	}
	if string(header[:3]) == "ID3" {
		size := synchsafeInt(header[6:10])
		if header[5]&0x10 != 0 {
			size += 10
		}
		if _, err := src.Seek(int64(10+size), io.SeekStart); err != nil {
			return 0, err
		}
		return io.Copy(dst, src)
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	_ = n
	return io.Copy(dst, src)
}

func synchsafeInt(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

type flvExtractInfo struct {
	AudioTags int
	MP3Bytes  int64
	Codecs    []string
}

func extractMP3FromFLVSource(src mediaSource, dest string) (flvExtractInfo, error) {
	in, err := src.Open()
	if err != nil {
		return flvExtractInfo{}, err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return flvExtractInfo{}, err
	}
	defer out.Close()
	return extractMP3FromFLV(in, out)
}

func extractMP3FromFLV(r io.Reader, w io.Writer) (flvExtractInfo, error) {
	var info flvExtractInfo
	codecs := map[string]bool{}
	br := bufio.NewReaderSize(r, 128*1024)
	header := make([]byte, 9)
	if _, err := io.ReadFull(br, header); err != nil {
		return info, err
	}
	if string(header[:3]) != "FLV" {
		return info, errors.New("FLV header پیدا نشد")
	}
	dataOffset := binary.BigEndian.Uint32(header[5:9])
	if dataOffset > 9 {
		if _, err := io.CopyN(io.Discard, br, int64(dataOffset-9)); err != nil {
			return info, err
		}
	}
	if _, err := io.CopyN(io.Discard, br, 4); err != nil {
		return info, err
	}

	for {
		tagHeader := make([]byte, 11)
		_, err := io.ReadFull(br, tagHeader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return info, err
		}
		tagType := tagHeader[0]
		dataSize := int(tagHeader[1])<<16 | int(tagHeader[2])<<8 | int(tagHeader[3])
		if dataSize < 0 || dataSize > 128*1024*1024 {
			return info, fmt.Errorf("اندازه tag نامعتبر است: %d", dataSize)
		}
		if tagType == 8 {
			if dataSize == 0 {
				if _, err := io.CopyN(io.Discard, br, 4); err != nil {
					return info, err
				}
				continue
			}
			first, err := br.ReadByte()
			if err != nil {
				return info, err
			}
			format := first >> 4
			info.AudioTags++
			switch format {
			case 2:
				n, err := io.CopyN(w, br, int64(dataSize-1))
				info.MP3Bytes += n
				if err != nil {
					return info, err
				}
				codecs["MP3"] = true
			case 10:
				codecs["AAC"] = true
				if _, err := io.CopyN(io.Discard, br, int64(dataSize-1)); err != nil {
					return info, err
				}
			case 11:
				codecs["Speex"] = true
				if _, err := io.CopyN(io.Discard, br, int64(dataSize-1)); err != nil {
					return info, err
				}
			case 4, 5, 6:
				codecs["Nellymoser"] = true
				if _, err := io.CopyN(io.Discard, br, int64(dataSize-1)); err != nil {
					return info, err
				}
			default:
				codecs[fmt.Sprintf("FLV-audio-%d", format)] = true
				if _, err := io.CopyN(io.Discard, br, int64(dataSize-1)); err != nil {
					return info, err
				}
			}
		} else {
			if _, err := io.CopyN(io.Discard, br, int64(dataSize)); err != nil {
				return info, err
			}
		}
		if _, err := io.CopyN(io.Discard, br, 4); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return info, err
		}
	}

	for codec := range codecs {
		info.Codecs = append(info.Codecs, codec)
	}
	sort.Strings(info.Codecs)
	return info, nil
}

func safeOutputName(raw string) string {
	u, err := url.Parse(raw)
	name := "acgir-audio"
	if err == nil {
		p := strings.TrimRight(u.Path, "/")
		if p != "" {
			name = path.Base(p)
			name = strings.TrimSuffix(name, path.Ext(name))
		}
	}
	name = sanitizeFileName(name)
	if name == "" {
		name = "acgir-audio"
	}
	if !strings.HasPrefix(strings.ToLower(name), "acgir") {
		name = "acgir-" + name
	}
	return name + ".mp3"
}

func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, "/", "-")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), ".-_")
}

func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB"}
	f := float64(n)
	for _, unit := range units {
		f /= 1024
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return fmt.Sprintf("%.1f TB", f/1024)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const indexHTML = `<!doctype html>
<html lang="fa" dir="rtl">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>ACGir</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f5f7f9;
      --panel: #ffffff;
      --text: #14202b;
      --muted: #5f6f80;
      --border: #d8e0e8;
      --accent: #1d6f86;
      --accent-strong: #13576a;
      --danger: #b83232;
      --ok: #23734c;
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Tahoma, Arial, sans-serif;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      background: var(--bg);
      color: var(--text);
    }
    main {
      width: min(920px, calc(100vw - 32px));
      margin: 0 auto;
      padding: 40px 0;
    }
    header {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 16px;
      margin-bottom: 18px;
    }
    h1 {
      margin: 0;
      font-size: 28px;
      letter-spacing: 0;
    }
    .version {
      color: var(--muted);
      font-size: 13px;
      white-space: nowrap;
    }
    section {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 18px;
      box-shadow: 0 10px 24px rgba(20, 32, 43, 0.06);
    }
    form {
      display: grid;
      gap: 14px;
    }
    label {
      display: grid;
      gap: 8px;
      color: var(--muted);
      font-size: 14px;
    }
    input, textarea {
      width: 100%;
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 12px 13px;
      font: inherit;
      color: var(--text);
      background: #fff;
      outline: none;
      direction: ltr;
      text-align: left;
    }
    textarea {
      min-height: 86px;
      resize: vertical;
    }
    input:focus, textarea:focus {
      border-color: var(--accent);
      box-shadow: 0 0 0 3px rgba(29, 111, 134, 0.12);
    }
    details {
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 11px 13px;
      background: #fbfcfd;
    }
    summary {
      cursor: pointer;
      color: var(--text);
      font-size: 14px;
      user-select: none;
    }
    details label {
      margin-top: 12px;
    }
    .row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      flex-wrap: wrap;
    }
    button, a.button {
      appearance: none;
      border: 0;
      border-radius: 8px;
      background: var(--accent);
      color: white;
      min-height: 44px;
      padding: 0 18px;
      font: inherit;
      font-weight: 700;
      cursor: pointer;
      text-decoration: none;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 8px;
    }
    button:hover, a.button:hover {
      background: var(--accent-strong);
    }
    button:disabled {
      cursor: wait;
      opacity: 0.65;
    }
    .note {
      color: var(--muted);
      font-size: 13px;
      line-height: 1.8;
      margin: 0;
    }
    .status {
      margin-top: 16px;
      display: none;
      gap: 12px;
    }
    .status.visible {
      display: grid;
    }
    .bar {
      width: 100%;
      height: 10px;
      border-radius: 999px;
      overflow: hidden;
      background: #e8edf2;
    }
    .fill {
      height: 100%;
      width: 0%;
      background: var(--accent);
      transition: width 180ms ease;
    }
    .step {
      color: var(--text);
      font-weight: 700;
      min-height: 24px;
    }
    .error {
      display: none;
      color: var(--danger);
      border: 1px solid rgba(184, 50, 50, 0.25);
      background: rgba(184, 50, 50, 0.06);
      border-radius: 8px;
      padding: 12px;
      line-height: 1.8;
      white-space: pre-wrap;
    }
    .error.visible {
      display: block;
    }
    .done {
      display: none;
    }
    .done.visible {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      flex-wrap: wrap;
      border: 1px solid rgba(35, 115, 76, 0.28);
      background: rgba(35, 115, 76, 0.07);
      border-radius: 8px;
      padding: 12px;
    }
    .done strong {
      color: var(--ok);
    }
    pre {
      margin: 0;
      max-height: 260px;
      overflow: auto;
      direction: rtl;
      text-align: right;
      white-space: pre-wrap;
      border: 1px solid var(--border);
      border-radius: 8px;
      background: #fbfcfd;
      color: #34495a;
      padding: 12px;
      font: 12px/1.7 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    }
    @media (max-width: 640px) {
      main { width: min(100vw - 22px, 920px); padding: 22px 0; }
      header { align-items: flex-start; flex-direction: column; gap: 6px; }
      section { padding: 14px; }
      button, a.button { width: 100%; }
      .row { align-items: stretch; }
    }
  </style>
</head>
<body>
  <main>
    <header>
      <h1>ACGir</h1>
      <div class="version">Adobe Connect Audio Extractor</div>
    </header>

    <section>
      <form id="form">
        <label>
          لینک ضبط Adobe Connect
          <input id="url" type="url" placeholder="https://server.example.com/p123456/" required autocomplete="off">
        </label>

        <details>
          <summary>نشست خصوصی / Cookie</summary>
          <label>
            Cookie اختیاری
            <textarea id="cookie" placeholder="BREEZESESSION=...; other=value"></textarea>
          </label>
        </details>

        <div class="row">
          <p class="note">فقط برای ضبط‌هایی استفاده کنید که اجازه دسترسی و دریافت آن‌ها را دارید. اگر سرور ورود بخواهد، Cookie همان نشست را وارد کنید.</p>
          <button id="submit" type="submit">ساخت MP3</button>
        </div>
      </form>

      <div id="status" class="status">
        <div class="bar"><div id="fill" class="fill"></div></div>
        <div id="step" class="step"></div>
        <div id="done" class="done">
          <strong>فایل آماده است.</strong>
          <a id="download" class="button" href="#">دریافت MP3</a>
        </div>
        <div id="error" class="error"></div>
        <pre id="logs"></pre>
      </div>
    </section>
  </main>

  <script>
    const form = document.getElementById('form');
    const urlInput = document.getElementById('url');
    const cookieInput = document.getElementById('cookie');
    const submit = document.getElementById('submit');
    const statusBox = document.getElementById('status');
    const fill = document.getElementById('fill');
    const step = document.getElementById('step');
    const logs = document.getElementById('logs');
    const errorBox = document.getElementById('error');
    const doneBox = document.getElementById('done');
    const download = document.getElementById('download');
    let pollTimer = null;

    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      clearInterval(pollTimer);
      submit.disabled = true;
      errorBox.classList.remove('visible');
      doneBox.classList.remove('visible');
      logs.textContent = '';
      step.textContent = 'شروع';
      fill.style.width = '0%';
      statusBox.classList.add('visible');

      try {
        const res = await fetch('/api/convert', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ url: urlInput.value.trim(), cookie: cookieInput.value.trim() })
        });
        const data = await res.json();
        if (!res.ok) throw new Error(data.error || 'درخواست پذیرفته نشد.');
        render(data);
        pollTimer = setInterval(() => poll(data.id), 900);
      } catch (err) {
        showError(err.message);
        submit.disabled = false;
      }
    });

    async function poll(id) {
      try {
        const res = await fetch('/api/jobs/' + encodeURIComponent(id));
        const data = await res.json();
        if (!res.ok) throw new Error('وضعیت کار خوانده نشد.');
        render(data);
        if (data.state === 'done' || data.state === 'error') {
          clearInterval(pollTimer);
          submit.disabled = false;
        }
      } catch (err) {
        clearInterval(pollTimer);
        submit.disabled = false;
        showError(err.message);
      }
    }

    function render(data) {
      fill.style.width = Math.round((data.progress || 0) * 100) + '%';
      step.textContent = data.step || '';
      logs.textContent = (data.logs || []).join('\n');
      logs.scrollTop = logs.scrollHeight;
      if (data.state === 'error') {
        showError(data.error || 'خطای ناشناخته');
      }
      if (data.state === 'done') {
        errorBox.classList.remove('visible');
        doneBox.classList.add('visible');
        download.href = data.downloadUrl;
        download.download = data.filename || 'acgir-audio.mp3';
      }
    }

    function showError(message) {
      errorBox.textContent = message;
      errorBox.classList.add('visible');
    }
  </script>
</body>
</html>`
