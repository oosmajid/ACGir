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
	mux.HandleFunc("/api/download-all", manager.handleDownloadAll)

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

func (m *jobManager) handleDownloadAll(w http.ResponseWriter, r *http.Request) {
	rawIDs := r.URL.Query().Get("ids")
	type entry struct {
		name string
		path string
	}
	var entries []entry
	for _, id := range strings.Split(rawIDs, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		j := m.get(id)
		if j == nil {
			continue
		}
		status := j.snapshot()
		if status.State != "done" || j.outputPath == "" {
			continue
		}
		if _, err := os.Stat(j.outputPath); err != nil {
			continue
		}
		name := status.Filename
		if name == "" {
			name = "acgir-audio.mp3"
		}
		entries = append(entries, entry{name: name, path: j.outputPath})
	}
	if len(entries) == 0 {
		http.Error(w, "no ready files", http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "acgir-recordings.zip"}))
	zw := zip.NewWriter(w)
	defer zw.Close()

	used := map[string]bool{}
	for _, e := range entries {
		name := uniqueZipName(e.name, used)
		in, err := os.Open(e.path)
		if err != nil {
			continue
		}
		out, err := zw.Create(name)
		if err != nil {
			in.Close()
			return
		}
		_, copyErr := io.Copy(out, in)
		in.Close()
		if copyErr != nil {
			return
		}
	}
}

func uniqueZipName(name string, used map[string]bool) string {
	if name == "" {
		name = "acgir-audio.mp3"
	}
	if !used[name] {
		used[name] = true
		return name
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
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
				j.fail(fmt.Errorf("این ضبط خصوصیه و برای باز شدن به «کوکی ورود» نیاز داره؛ لینک به صفحهٔ ورود سامانه رسید. توی فرم، بخش «ضبط خصوصیه و باز نمی‌شه؟» رو باز کن و طبق راهنمای قدم‌به‌قدم، کوکی ورود رو وارد کن و دوباره امتحان کن. (آدرسی که به صفحهٔ ورود رسید: %s)", discovery.LoginURL))
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
		return nil, errors.New("نتونستم محتوای این ضبط رو پیدا کنم. اگر ضبط خصوصیه، توی بخش «ضبط خصوصیه و باز نمی‌شه؟» کوکی ورود رو وارد کن و دوباره امتحان کن.")
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
		return nil, errors.New("هیچ فایل صوتی‌ای دانلود نشد. اگر این کلاس نیاز به ورود داره، توی بخش «ضبط خصوصیه و باز نمی‌شه؟» کوکی ورود رو وارد کن و دوباره امتحان کن.")
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
		base := strings.ToLower(path.Base(strings.TrimRight(u.Path, "/")))
		if base == "" || strings.HasSuffix(base, ".php") {
			// Connector/launcher links (e.g. joinrecording.php) share the same
			// path, so derive the name from a distinguishing query parameter.
			for _, key := range []string{"id", "sco-id", "scoId", "recordingId", "session"} {
				if v := strings.TrimSpace(u.Query().Get(key)); v != "" {
					name = "recording-" + v
					break
				}
			}
		} else {
			p := strings.TrimRight(u.Path, "/")
			if p != "" {
				name = path.Base(p)
				name = strings.TrimSuffix(name, path.Ext(name))
			}
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
  <title>ACGir — صدای کلاس را به MP3 تبدیل کن</title>
  <meta name="theme-color" content="#0e7d92" media="(prefers-color-scheme: light)">
  <meta name="theme-color" content="#0a0f16" media="(prefers-color-scheme: dark)">
  <link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 64 64'><rect width='64' height='64' rx='16' fill='%230e7d92'/><g fill='white'><rect x='14' y='25' width='5' height='14' rx='2.5'/><rect x='25' y='15' width='5' height='34' rx='2.5'/><rect x='36' y='28' width='5' height='8' rx='2.5'/><rect x='47' y='22' width='5' height='20' rx='2.5'/></g></svg>">
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Vazirmatn:wght@400;500;600;700&display=swap" rel="stylesheet">
  <style>
    :root {
      color-scheme: light dark;
      --bg-1: #e9eff6;
      --bg-2: #f5f8fc;
      --surface: #ffffff;
      --surface-soft: #f5f8fc;
      --text: #102031;
      --muted: #566778;
      --faint: #8090a0;
      --border: #e4eaf1;
      --border-strong: #d3dde8;
      --brand: #0e7d92;
      --brand-strong: #0a6172;
      --brand-tint: rgba(14, 125, 146, 0.10);
      --on-brand: #ffffff;
      --ring: rgba(14, 125, 146, 0.28);
      --ok-text: #14683d;
      --ok-bg: #e7f5ee;
      --ok-border: #bce4cd;
      --err-text: #a72822;
      --err-bg: #fceceb;
      --err-border: #f3c9c6;
      --info-text: #1f4fd0;
      --info-bg: #eaf1ff;
      --info-border: #d2e0ff;
      --shadow: 0 18px 40px -20px rgba(16, 32, 47, 0.30), 0 4px 12px -6px rgba(16, 32, 47, 0.10);
      --shadow-sm: 0 2px 8px -4px rgba(16, 32, 47, 0.18);
      --radius: 16px;
      --font: "Vazirmatn", "Segoe UI", Tahoma, system-ui, -apple-system, BlinkMacSystemFont, sans-serif;
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg-1: #090e15;
        --bg-2: #0f1620;
        --surface: #141d29;
        --surface-soft: #101822;
        --text: #e8eef5;
        --muted: #9babbd;
        --faint: #6e8094;
        --border: #21303f;
        --border-strong: #2c3d4f;
        --brand: #25b3c6;
        --brand-strong: #1796a9;
        --brand-tint: rgba(37, 179, 198, 0.14);
        --on-brand: #04222a;
        --ring: rgba(37, 179, 198, 0.32);
        --ok-text: #7ee3ab;
        --ok-bg: rgba(33, 130, 80, 0.16);
        --ok-border: rgba(33, 130, 80, 0.40);
        --err-text: #ff9b94;
        --err-bg: rgba(207, 59, 52, 0.16);
        --err-border: rgba(207, 59, 52, 0.42);
        --info-text: #9cc0ff;
        --info-bg: rgba(37, 99, 235, 0.14);
        --info-border: rgba(37, 99, 235, 0.38);
        --shadow: 0 22px 48px -24px rgba(0, 0, 0, 0.70), 0 4px 14px -8px rgba(0, 0, 0, 0.50);
        --shadow-sm: 0 2px 10px -6px rgba(0, 0, 0, 0.6);
      }
    }
    * { box-sizing: border-box; }
    html { -webkit-text-size-adjust: 100%; }
    body {
      margin: 0;
      min-height: 100dvh;
      font-family: var(--font);
      font-size: 16px;
      line-height: 1.7;
      color: var(--text);
      background:
        radial-gradient(1100px 520px at 85% -8%, var(--brand-tint), transparent 60%),
        linear-gradient(180deg, var(--bg-1), var(--bg-2));
      background-attachment: fixed;
    }
    .app {
      width: min(700px, calc(100vw - 32px));
      margin: 0 auto;
      padding: 28px 0 56px;
    }

    /* Header */
    .hero {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      flex-wrap: wrap;
      margin-bottom: 22px;
    }
    .brand { display: flex; align-items: center; gap: 14px; }
    .logo {
      flex: none;
      width: 52px;
      height: 52px;
      border-radius: 15px;
      display: grid;
      place-items: center;
      color: #fff;
      background: linear-gradient(150deg, #15a7bd, var(--brand-strong));
      box-shadow: var(--shadow-sm), inset 0 1px 0 rgba(255,255,255,0.18);
    }
    .logo svg { width: 28px; height: 28px; }
    .brand h1 { margin: 0; font-size: 25px; font-weight: 700; letter-spacing: -0.2px; }
    .brand p { margin: 3px 0 0; color: var(--muted); font-size: 13.5px; line-height: 1.6; max-width: 42ch; }
    .pill {
      flex: none;
      display: inline-flex;
      align-items: center;
      gap: 7px;
      font-size: 12.5px;
      font-weight: 600;
      color: var(--brand-strong);
      background: var(--brand-tint);
      border: 1px solid color-mix(in srgb, var(--brand) 22%, transparent);
      padding: 6px 12px;
      border-radius: 999px;
      white-space: nowrap;
    }
    .pill svg { width: 14px; height: 14px; }

    /* Panel + form */
    .panel {
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: var(--radius);
      padding: 22px;
      box-shadow: var(--shadow);
    }
    form { display: grid; gap: 18px; }
    .field { display: grid; gap: 9px; }
    .field > label { font-size: 14.5px; font-weight: 600; color: var(--text); }
    textarea, input {
      width: 100%;
      font: inherit;
      color: var(--text);
      background: var(--surface-soft);
      border: 1.5px solid var(--border-strong);
      border-radius: 12px;
      padding: 13px 14px;
      outline: none;
      transition: border-color 140ms ease, box-shadow 140ms ease, background 140ms ease;
    }
    #urls { min-height: 104px; resize: vertical; direction: ltr; text-align: left; line-height: 1.9; }
    #cookie { min-height: 78px; resize: vertical; direction: ltr; text-align: left; font-size: 13px; }
    textarea::placeholder { color: var(--faint); }
    textarea:focus, input:focus {
      border-color: var(--brand);
      background: var(--surface);
      box-shadow: 0 0 0 4px var(--ring);
    }
    .field-foot {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 10px;
      flex-wrap: wrap;
    }
    .hint { color: var(--muted); font-size: 12.5px; line-height: 1.6; }
    .counter { color: var(--brand-strong); font-size: 12.5px; font-weight: 700; white-space: nowrap; }
    .counter:empty { display: none; }

    /* Disclosure (cookie guide) */
    .disclosure {
      border: 1px solid var(--border);
      border-radius: 13px;
      background: var(--surface-soft);
      overflow: hidden;
    }
    .disclosure > summary {
      list-style: none;
      cursor: pointer;
      display: flex;
      align-items: center;
      gap: 11px;
      padding: 13px 15px;
      font-size: 14px;
      font-weight: 600;
      color: var(--text);
      user-select: none;
    }
    .disclosure > summary::-webkit-details-marker { display: none; }
    .summary-ico {
      flex: none; width: 30px; height: 30px; border-radius: 9px;
      display: grid; place-items: center;
      color: var(--brand-strong); background: var(--brand-tint);
    }
    .summary-ico svg { width: 16px; height: 16px; }
    .summary-txt { flex: 1 1 auto; }
    .summary-txt small { display: block; font-weight: 400; font-size: 12px; color: var(--muted); margin-top: 1px; }
    .chev { flex: none; width: 18px; height: 18px; color: var(--faint); transition: transform 200ms ease; }
    .disclosure[open] .chev { transform: rotate(180deg); }
    .disclosure-body { padding: 4px 15px 16px; border-top: 1px solid var(--border); display: grid; gap: 13px; }
    .lead { margin: 12px 0 2px; font-size: 13.5px; color: var(--muted); line-height: 1.8; }
    .lead b { color: var(--text); }
    .steps { margin: 0; padding: 0; list-style: none; display: grid; gap: 11px; counter-reset: s; }
    .steps li {
      position: relative;
      padding-inline-start: 40px;
      min-height: 28px;
      font-size: 13.5px;
      line-height: 1.85;
      color: var(--text);
      counter-increment: s;
    }
    .steps li::before {
      content: counter(s);
      position: absolute;
      inset-inline-start: 0;
      top: 1px;
      width: 27px;
      height: 27px;
      border-radius: 50%;
      display: grid;
      place-items: center;
      font-size: 13px;
      font-weight: 700;
      color: var(--on-brand);
      background: linear-gradient(150deg, #15a7bd, var(--brand-strong));
    }
    .steps kbd {
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: 12px;
      background: var(--surface);
      border: 1px solid var(--border-strong);
      border-bottom-width: 2px;
      border-radius: 6px;
      padding: 1px 6px;
      color: var(--text);
      direction: ltr;
      display: inline-block;
    }
    .cookie-label { font-size: 13px; font-weight: 600; color: var(--text); margin-top: 2px; }
    .privacy {
      margin: 0;
      display: flex;
      gap: 10px;
      align-items: flex-start;
      font-size: 12.5px;
      line-height: 1.75;
      color: var(--info-text);
      background: var(--info-bg);
      border: 1px solid var(--info-border);
      border-radius: 11px;
      padding: 11px 13px;
    }
    .privacy svg { flex: none; width: 17px; height: 17px; margin-top: 2px; }

    /* Actions */
    .actions {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 14px;
      flex-wrap: wrap;
    }
    .actions .note { margin: 0; color: var(--muted); font-size: 12.5px; line-height: 1.7; flex: 1 1 200px; }
    .btn {
      appearance: none;
      border: 0;
      cursor: pointer;
      font: inherit;
      font-weight: 700;
      border-radius: 12px;
      min-height: 48px;
      padding: 0 22px;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 9px;
      text-decoration: none;
      color: var(--on-brand);
      background: linear-gradient(150deg, #13a0b5, var(--brand-strong));
      box-shadow: var(--shadow-sm), inset 0 1px 0 rgba(255,255,255,0.16);
      transition: transform 120ms ease, filter 140ms ease, box-shadow 140ms ease, opacity 140ms ease;
    }
    .btn svg { width: 19px; height: 19px; }
    .btn:hover { filter: brightness(1.06); }
    .btn:active { transform: translateY(1px); }
    .btn:focus-visible { outline: none; box-shadow: 0 0 0 4px var(--ring); }
    .btn:disabled { cursor: progress; opacity: 0.6; filter: saturate(0.7); }
    .btn-ghost {
      color: var(--brand-strong);
      background: var(--surface);
      border: 1.5px solid var(--border-strong);
      box-shadow: none;
      min-height: 42px;
      font-size: 14px;
      padding: 0 16px;
    }
    .btn-ghost:hover { filter: none; background: var(--surface-soft); border-color: var(--brand); }
    .btn-sm { min-height: 38px; padding: 0 15px; font-size: 13.5px; border-radius: 10px; }

    /* Alerts */
    .alert {
      font-size: 13.5px;
      line-height: 1.8;
      border-radius: 12px;
      padding: 12px 14px;
      white-space: pre-wrap;
    }
    .alert[hidden] { display: none; }
    .alert-error { color: var(--err-text); background: var(--err-bg); border: 1px solid var(--err-border); }
    .panel > .alert-error { margin-top: 16px; }

    /* Results */
    .results { display: none; margin-top: 18px; }
    .results.visible { display: block; }
    .toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      flex-wrap: wrap;
      margin-bottom: 12px;
      padding: 0 4px;
    }
    .summary { color: var(--muted); font-size: 13.5px; font-weight: 600; }
    #cards { display: grid; gap: 12px; }

    /* Card */
    .card {
      background: var(--surface);
      border: 1px solid var(--border);
      border-inline-start: 4px solid var(--border-strong);
      border-radius: 13px;
      padding: 15px 16px;
      box-shadow: var(--shadow-sm);
      display: grid;
      gap: 12px;
      transition: border-color 160ms ease;
    }
    .card[data-state="running"] { border-inline-start-color: var(--brand); }
    .card[data-state="done"] { border-inline-start-color: var(--ok-text); }
    .card[data-state="error"] { border-inline-start-color: var(--err-text); }
    .card-top { display: flex; align-items: center; gap: 11px; }
    .idx {
      flex: none;
      width: 26px; height: 26px;
      border-radius: 8px;
      display: grid; place-items: center;
      font-size: 12.5px; font-weight: 700;
      color: var(--muted);
      background: var(--surface-soft);
      border: 1px solid var(--border);
    }
    .card-meta { flex: 1 1 auto; min-width: 0; display: grid; gap: 1px; }
    .link {
      direction: ltr; text-align: left;
      font: 12.5px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      color: var(--muted);
      overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    }
    .step { font-size: 13px; font-weight: 600; color: var(--text); min-height: 18px; }
    .badge {
      flex: none;
      display: inline-flex; align-items: center; gap: 6px;
      font-size: 12.5px; font-weight: 700;
      padding: 5px 11px;
      border-radius: 999px;
      white-space: nowrap;
      color: var(--muted);
      background: var(--surface-soft);
      border: 1px solid var(--border);
    }
    .badge svg { width: 14px; height: 14px; }
    .card[data-state="running"] .badge { color: var(--brand-strong); background: var(--brand-tint); border-color: transparent; }
    .card[data-state="done"] .badge { color: var(--ok-text); background: var(--ok-bg); border-color: var(--ok-border); }
    .card[data-state="error"] .badge { color: var(--err-text); background: var(--err-bg); border-color: var(--err-border); }
    .bar { width: 100%; height: 8px; border-radius: 999px; background: var(--surface-soft); border: 1px solid var(--border); overflow: hidden; }
    .fill {
      height: 100%; width: 0%;
      border-radius: 999px;
      background: linear-gradient(90deg, #15a7bd, var(--brand));
      transition: width 280ms ease;
    }
    .card[data-state="done"] .fill { background: linear-gradient(90deg, #1fa86a, var(--ok-text)); }
    .card[data-state="running"] .fill {
      background-image: linear-gradient(90deg, #15a7bd, var(--brand)), linear-gradient(110deg, transparent 30%, rgba(255,255,255,0.45) 50%, transparent 70%);
      background-size: 100% 100%, 220px 100%;
      background-repeat: no-repeat, no-repeat;
      animation: shimmer 1.25s linear infinite;
    }
    @keyframes shimmer { from { background-position: 0 0, -220px 0; } to { background-position: 0 0, calc(100% + 220px) 0; } }
    .card-actions { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
    .download[hidden] { display: none; }
    .logwrap { margin-inline-start: auto; }
    .logwrap > summary {
      list-style: none; cursor: pointer; user-select: none;
      font-size: 12.5px; color: var(--faint); font-weight: 600;
    }
    .logwrap > summary::-webkit-details-marker { display: none; }
    .logwrap > summary:hover { color: var(--muted); }
    .logs {
      margin: 10px 0 0;
      max-height: 190px; overflow: auto;
      direction: rtl; text-align: right; white-space: pre-wrap;
      background: var(--surface-soft);
      border: 1px solid var(--border);
      border-radius: 10px;
      padding: 11px 12px;
      color: var(--muted);
      font: 11.5px/1.75 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    }
    .card .alert-error { margin: 0; }

    /* Empty state */
    .empty {
      margin-top: 18px;
      text-align: center;
      padding: 34px 20px;
      border: 1.5px dashed var(--border-strong);
      border-radius: var(--radius);
      color: var(--muted);
    }
    .empty[hidden] { display: none; }
    .empty .art { width: 64px; height: 64px; margin: 0 auto 12px; color: var(--brand); opacity: 0.85; }
    .empty h3 { margin: 0 0 5px; font-size: 15.5px; font-weight: 700; color: var(--text); }
    .empty p { margin: 0 auto; font-size: 13px; max-width: 40ch; line-height: 1.8; }

    /* Footer */
    .foot {
      margin-top: 22px;
      text-align: center;
      font-size: 12.5px;
      color: var(--faint);
      display: flex; align-items: center; justify-content: center; gap: 7px;
    }
    .foot svg { width: 14px; height: 14px; }

    .spin { animation: spin 0.9s linear infinite; transform-origin: center; }
    @keyframes spin { to { transform: rotate(360deg); } }

    @media (max-width: 600px) {
      .app { width: calc(100vw - 24px); padding: 18px 0 40px; }
      .hero { gap: 12px; }
      .panel { padding: 17px; }
      .btn { width: 100%; }
      .actions .note { flex-basis: 100%; }
      .toolbar .btn-ghost { width: 100%; }
    }
    @media (prefers-reduced-motion: reduce) {
      * { animation-duration: 0.001ms !important; animation-iteration-count: 1 !important; transition-duration: 0.001ms !important; }
    }
  </style>
</head>
<body>
  <div class="app">
    <header class="hero">
      <div class="brand">
        <div class="logo" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="currentColor"><rect x="2.5" y="9" width="3" height="6" rx="1.5"/><rect x="7.5" y="5" width="3" height="14" rx="1.5"/><rect x="12.5" y="10.5" width="3" height="3" rx="1.5"/><rect x="17.5" y="7" width="3" height="10" rx="1.5"/></svg>
        </div>
        <div class="brand-text">
          <h1>ACGir</h1>
          <p>صدای کلاس‌های ضبط‌شدهٔ ادوبی‌کانکت رو در چند ثانیه به فایل MP3 تبدیل کن.</p>
        </div>
      </div>
      <span class="pill">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z"/><path d="m9 12 2 2 4-4"/></svg>
        روی کامپیوتر خودت اجرا می‌شه
      </span>
    </header>

    <main>
      <section class="panel">
        <form id="form" novalidate>
          <div class="field">
            <label for="urls">لینک کلاس‌هایی که می‌خوای صداشون رو داشته باشی</label>
            <textarea id="urls" autocomplete="off" spellcheck="false" placeholder="https://example.edu/p123456/&#10;https://example.edu/p789012/"></textarea>
            <div class="field-foot">
              <span class="hint">هر خط یک لینک. می‌تونی یکی بذاری یا چند تا با هم — برای هر کدوم یک فایل جدا می‌گیری.</span>
              <span id="counter" class="counter"></span>
            </div>
          </div>

          <details class="disclosure">
            <summary>
              <span class="summary-ico" aria-hidden="true">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
              </span>
              <span class="summary-txt">
                ضبط خصوصیه و باز نمی‌شه؟
                <small>اگر برنامه گفت لینک به «صفحهٔ ورود» می‌ره، اینجا رو باز کن</small>
              </span>
              <svg class="chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg>
            </summary>
            <div class="disclosure-body">
              <p class="lead"><b>بیشتر ضبط‌ها بدون این مرحله کار می‌کنن.</b> فقط اگر دیدی باز نمی‌شه و برنامه گفت به صفحهٔ ورود می‌رسه، این چند قدم ساده رو برو تا «کوکی ورود» رو پیدا کنی:</p>
              <ol class="steps">
                <li>توی همین مرورگر، اول وارد سامانهٔ دانشگاه یا کلاست شو — همون‌جایی که معمولاً ضبط‌ها رو می‌بینی.</li>
                <li>صفحهٔ همون ضبط رو باز کن، بعد کلید <kbd>F12</kbd> رو بزن (یا کلیک راست → «Inspect / بازرسی»).</li>
                <li>بالای پنجره‌ای که باز شد، تب <kbd>Network</kbd> رو انتخاب کن و یک‌بار صفحه رو رفرش کن (<kbd>F5</kbd>).</li>
                <li>روی اولین ردیف لیست کلیک کن و پایین، توی بخش «Request Headers»، دنبال خطی بگرد که با <kbd>Cookie:</kbd> شروع می‌شه.</li>
                <li>تمام متن جلوی Cookie رو کپی کن و توی کادر پایین بچسبون. (بخشی که با <kbd>BREEZESESSION</kbd> شروع می‌شه از همه مهم‌تره.)</li>
              </ol>
              <label class="cookie-label" for="cookie">متن کوکی رو اینجا بچسبون</label>
              <textarea id="cookie" autocomplete="off" spellcheck="false" placeholder="BREEZESESSION=...; نام=مقدار؛ ..."></textarea>
              <p class="privacy">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z"/><path d="m9 12 2 2 4-4"/></svg>
                <span>خیالت راحت: این کوکی فقط روی همین کامپیوتر و فقط برای همین دانلود استفاده می‌شه. هیچ‌جا ذخیره یا ارسال نمی‌شه و با بستن برنامه پاک می‌شه.</span>
              </p>
            </div>
          </details>

          <div class="actions">
            <p class="note">فقط ضبط‌هایی رو دانلود کن که اجازهٔ دسترسی بهشون رو داری.</p>
            <button id="submit" class="btn" type="submit">
              <svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="2.5" y="9" width="3" height="6" rx="1.5"/><rect x="7.5" y="5" width="3" height="14" rx="1.5"/><rect x="12.5" y="10.5" width="3" height="3" rx="1.5"/><rect x="17.5" y="7" width="3" height="10" rx="1.5"/></svg>
              تبدیل به MP3
            </button>
          </div>
        </form>

        <div id="formError" class="alert alert-error" role="alert" hidden></div>
      </section>

      <section id="results" class="results" aria-live="polite">
        <div class="toolbar">
          <span id="summary" class="summary"></span>
          <a id="downloadAll" class="btn btn-ghost" href="#" hidden>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
            دریافت همه در یک فایل zip
          </a>
        </div>
        <div id="cards"></div>
      </section>

      <div id="empty" class="empty">
        <svg class="art" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><line x1="3" y1="12" x2="3" y2="14"/><line x1="6.5" y1="9" x2="6.5" y2="17"/><line x1="10" y1="5" x2="10" y2="21"/><line x1="13.5" y1="8" x2="13.5" y2="18"/><line x1="17" y1="10" x2="17" y2="15"/><line x1="20.5" y1="11" x2="20.5" y2="13"/></svg>
        <h3>هنوز لینکی اضافه نکردی</h3>
        <p>لینک کلاس‌ها رو بالا بذار و دکمهٔ «تبدیل به MP3» رو بزن تا فایل‌ها همین‌جا، یکی‌یکی، آماده بشن.</p>
      </div>

      <p class="foot">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
        این برنامه روی کامپیوتر خودت اجرا می‌شه و فقط برای دانلود ضبط به سرور کلاست وصل می‌شه.
      </p>
    </main>
  </div>

  <script>
    var MAX_CONCURRENT = 3;
    var POLL_MS = 1000;
    function $(id) { return document.getElementById(id); }

    var form = $('form'), urlsInput = $('urls'), cookieInput = $('cookie'),
        submit = $('submit'), formError = $('formError'), results = $('results'),
        cardsBox = $('cards'), summary = $('summary'), downloadAll = $('downloadAll'),
        counter = $('counter'), empty = $('empty');

    var jobs = [], pollTimer = null;

    var ICONS = {
      queued: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><polyline points="12 7 12 12 15.5 14"/></svg>',
      running: '<svg class="spin" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round"><path d="M21 12a9 9 0 1 1-6.2-8.55"/></svg>',
      done: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>',
      error: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><line x1="12" y1="8" x2="12" y2="13"/><line x1="12" y1="16.5" x2="12.01" y2="16.5"/></svg>'
    };
    var DLICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>';
    var LABELS = { queued: 'در صف', running: 'در حال پردازش', done: 'آماده', error: 'مشکل پیش اومد' };

    function toFa(n) {
      return String(n).replace(/[0-9]/g, function (d) { return '۰۱۲۳۴۵۶۷۸۹'.charAt(+d); });
    }

    function parseURLs(text) {
      var seen = {}, out = [];
      var lines = text.split(/\r?\n/);
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i].trim();
        if (!line || !/^https?:\/\//i.test(line) || seen[line]) continue;
        seen[line] = true;
        out.push(line);
      }
      return out;
    }

    function updateCounter() {
      var n = parseURLs(urlsInput.value).length;
      counter.textContent = n ? (toFa(n) + (n === 1 ? ' لینک' : ' لینک')) : '';
    }
    urlsInput.addEventListener('input', updateCounter);

    form.addEventListener('submit', function (event) {
      event.preventDefault();
      formError.hidden = true;

      var urls = parseURLs(urlsInput.value);
      if (urls.length === 0) {
        formError.textContent = 'یک لینک معتبر بذار که با http یا https شروع بشه. مثلاً: https://example.edu/p123456/';
        formError.hidden = false;
        return;
      }

      if (pollTimer) clearInterval(pollTimer);
      cardsBox.textContent = '';
      empty.hidden = true;
      jobs = urls.map(function (url, i) { return createJob(url, i + 1); });
      results.classList.add('visible');
      submit.disabled = true;

      pump();
      updateToolbar();
      pollTimer = setInterval(pollAll, POLL_MS);
    });

    function createJob(url, idx) {
      var card = document.createElement('article');
      card.className = 'card';
      card.innerHTML =
        '<div class="card-top">' +
          '<span class="idx"></span>' +
          '<div class="card-meta"><span class="link" dir="ltr" title=""></span><span class="step"></span></div>' +
          '<span class="badge"></span>' +
        '</div>' +
        '<div class="bar"><div class="fill"></div></div>' +
        '<div class="card-actions">' +
          '<a class="download btn btn-sm" href="#" hidden></a>' +
          '<details class="logwrap"><summary>جزئیات</summary><pre class="logs"></pre></details>' +
        '</div>' +
        '<div class="error alert alert-error" hidden></div>';
      card.querySelector('.idx').textContent = toFa(idx);
      var linkEl = card.querySelector('.link');
      linkEl.textContent = url;
      linkEl.title = url;
      cardsBox.appendChild(card);

      var job = {
        url: url, idx: idx, id: null, state: 'idle',
        el: {
          card: card,
          badge: card.querySelector('.badge'),
          fill: card.querySelector('.fill'),
          step: card.querySelector('.step'),
          download: card.querySelector('.download'),
          error: card.querySelector('.error'),
          logs: card.querySelector('.logs')
        }
      };
      applyState(job, 'queued', 'برای شروع در نوبت است');
      return job;
    }

    function applyState(job, kind, stepText) {
      job.el.card.dataset.state = kind;
      job.el.badge.innerHTML = ICONS[kind] + '<span>' + LABELS[kind] + '</span>';
      if (typeof stepText === 'string') job.el.step.textContent = stepText;
    }

    function pump() {
      var active = jobs.filter(function (j) { return j.state === 'starting' || j.state === 'running'; }).length;
      var slots = MAX_CONCURRENT - active;
      for (var i = 0; i < jobs.length && slots > 0; i++) {
        if (jobs[i].state === 'idle') { startJob(jobs[i]); slots--; }
      }
    }

    function startJob(job) {
      job.state = 'starting';
      applyState(job, 'running', 'در حال شروع…');
      fetch('/api/convert', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: job.url, cookie: cookieInput.value.trim() })
      }).then(function (res) {
        return res.json().then(function (data) {
          if (!res.ok) throw new Error(data.error || 'درخواست پذیرفته نشد.');
          job.id = data.id;
          job.state = 'running';
          render(job, data);
        });
      }).catch(function (err) {
        job.state = 'error';
        renderError(job, err.message);
        pump(); updateToolbar(); checkAllDone();
      });
    }

    function pollAll() {
      var running = jobs.filter(function (j) { return j.state === 'running' && j.id; });
      Promise.all(running.map(function (job) {
        return fetch('/api/jobs/' + encodeURIComponent(job.id))
          .then(function (res) {
            return res.json().then(function (data) {
              if (!res.ok) throw new Error('وضعیت کار خوانده نشد.');
              render(job, data);
            });
          })
          .catch(function (err) { job.state = 'error'; renderError(job, err.message); });
      })).then(function () {
        pump(); updateToolbar(); checkAllDone();
      });
    }

    function render(job, data) {
      job.el.fill.style.width = Math.round((data.progress || 0) * 100) + '%';
      job.el.logs.textContent = (data.logs || []).join('\n');
      job.el.logs.scrollTop = job.el.logs.scrollHeight;

      if (data.state === 'done') {
        job.state = 'done';
        job.el.fill.style.width = '100%';
        applyState(job, 'done', 'فایل MP3 آماده‌ست');
        job.el.error.hidden = true;
        job.el.download.hidden = false;
        job.el.download.href = data.downloadUrl;
        job.el.download.download = data.filename || 'acgir-audio.mp3';
        job.el.download.innerHTML = DLICON + '<span>دریافت ' + (data.filename || 'MP3') + '</span>';
      } else if (data.state === 'error') {
        job.state = 'error';
        renderError(job, data.error || 'خطای ناشناخته');
      } else {
        applyState(job, 'running', data.step || 'در حال پردازش…');
      }
    }

    function renderError(job, message) {
      applyState(job, 'error', '');
      job.el.step.textContent = '';
      job.el.error.textContent = message;
      job.el.error.hidden = false;
    }

    function updateToolbar() {
      var done = jobs.filter(function (j) { return j.state === 'done'; });
      var errored = jobs.filter(function (j) { return j.state === 'error'; }).length;
      var pending = jobs.length - done.length - errored;
      var parts = [toFa(jobs.length) + ' لینک'];
      if (done.length) parts.push(toFa(done.length) + ' آماده');
      if (pending > 0) parts.push(toFa(pending) + ' در حال انجام');
      if (errored) parts.push(toFa(errored) + ' خطا');
      summary.textContent = parts.join('  ·  ');

      if (done.length > 1) {
        downloadAll.hidden = false;
        downloadAll.href = '/api/download-all?ids=' + done.map(function (j) { return encodeURIComponent(j.id); }).join(',');
      } else {
        downloadAll.hidden = true;
      }
    }

    function checkAllDone() {
      var going = jobs.some(function (j) { return j.state === 'idle' || j.state === 'starting' || j.state === 'running'; });
      if (!going) {
        if (pollTimer) clearInterval(pollTimer);
        pollTimer = null;
        submit.disabled = false;
      }
    }
  </script>
</body>
</html>`
