package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractMP3FromFLV(t *testing.T) {
	payload := []byte{0xff, 0xfb, 0x90, 0x64, 0x00, 0x0f, 0xf0}
	flv := buildAudioFLV(2, payload)

	var out bytes.Buffer
	info, err := extractMP3FromFLV(bytes.NewReader(flv), &out)
	if err != nil {
		t.Fatalf("extractMP3FromFLV returned error: %v", err)
	}
	if info.MP3Bytes != int64(len(payload)) {
		t.Fatalf("MP3Bytes = %d, want %d", info.MP3Bytes, len(payload))
	}
	if got := out.Bytes(); !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %x want %x", got, payload)
	}
}

func TestExtractMP3FromFLVReportsUnsupportedAAC(t *testing.T) {
	flv := buildAudioFLV(10, []byte{0x00, 0x12, 0x10})

	var out bytes.Buffer
	info, err := extractMP3FromFLV(bytes.NewReader(flv), &out)
	if err != nil {
		t.Fatalf("extractMP3FromFLV returned error: %v", err)
	}
	if info.MP3Bytes != 0 {
		t.Fatalf("MP3Bytes = %d, want 0", info.MP3Bytes)
	}
	if len(info.Codecs) != 1 || info.Codecs[0] != "AAC" {
		t.Fatalf("Codecs = %#v, want AAC", info.Codecs)
	}
}

func TestDownloadZipAndExtractEndToEnd(t *testing.T) {
	payload := []byte{0xff, 0xfb, 0x90, 0x64, 0x11, 0x22, 0x33}
	zipBody := buildRecordingZip(t, "cameraVoip_0_1.flv", buildAudioFLV(2, payload))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/p123/" && r.URL.Query().Get("mode") == "xml":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<results><sco sco-id="42"><url-path>/p123/</url-path></sco></results>`))
		case r.URL.Path == "/p123/output/lecture.zip" && r.URL.Query().Get("download") == "zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	workDir := t.TempDir()
	j := &job{workDir: workDir, status: jobStatus{ID: "test", Logs: []string{}}}
	discovery := discover(server.URL+"/p123/", server.Client(), "", j)
	if len(discovery.ZipCandidates) == 0 {
		t.Fatal("no zip candidates discovered")
	}

	zipPath, _, err := downloadFirstZip(server.Client(), discovery.ZipCandidates, "", workDir, j)
	if err != nil {
		t.Fatalf("downloadFirstZip returned error: %v", err)
	}
	output := filepath.Join(workDir, "audio.mp3")
	result, err := extractFromZip(zipPath, workDir, output, j)
	if err != nil {
		t.Fatalf("extractFromZip returned error: %v", err)
	}
	if result.Bytes != int64(len(payload)) {
		t.Fatalf("result.Bytes = %d, want %d", result.Bytes, len(payload))
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile output: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("output mismatch: got %x want %x", got, payload)
	}
}

func TestZipCandidateMatchesLegacyLectureURLAndSession(t *testing.T) {
	base := recordingBaseFromURL("https://example.edu/p123/?session=abc123&ignored=1")
	candidates := zipCandidatesForRecordingBase(base)
	if len(candidates) == 0 {
		t.Fatal("no candidates")
	}
	want := "https://example.edu/p123/output/lecture.zip?download=zip&session=abc123"
	if candidates[0] != want {
		t.Fatalf("first candidate = %q, want %q", candidates[0], want)
	}
}

func TestLaunchZipCandidatePreservesConnectorQuery(t *testing.T) {
	candidates := zipCandidatesForLaunchURL("https://binaloud.dpm.ir/mod/adobeconnect/joinrecording.php?id=42&session=abc123")
	if len(candidates) < 2 {
		t.Fatalf("len(candidates) = %d, want at least 2", len(candidates))
	}
	wantFirst := "https://binaloud.dpm.ir/mod/adobeconnect/joinrecording.php/output/lecture.zip?download=zip&id=42&session=abc123"
	if candidates[0] != wantFirst {
		t.Fatalf("first candidate = %q, want %q", candidates[0], wantFirst)
	}
	wantSecond := "https://binaloud.dpm.ir/mod/adobeconnect/joinrecording.php/output/lecture.zip?download=zip&session=abc123"
	if candidates[1] != wantSecond {
		t.Fatalf("second candidate = %q, want %q", candidates[1], wantSecond)
	}
}

func TestDiscoverConnectorRedirectsToRecordingBase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mod/adobeconnect/joinrecording.php":
			http.Redirect(w, r, "/p123/?session=abc123", http.StatusFound)
		case "/p123/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>recording</body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	j := &job{status: jobStatus{ID: "test", Logs: []string{}}}
	info := discover(server.URL+"/mod/adobeconnect/joinrecording.php?id=42", server.Client(), "", j)
	if len(info.RecordingBases) != 1 {
		t.Fatalf("RecordingBases = %#v, want one redirected base", info.RecordingBases)
	}
	wantBase := server.URL + "/p123/?session=abc123"
	if info.RecordingBases[0] != wantBase {
		t.Fatalf("base = %q, want %q", info.RecordingBases[0], wantBase)
	}
	wantZip := server.URL + "/p123/output/lecture.zip?download=zip&session=abc123"
	found := false
	for _, candidate := range info.ZipCandidates {
		if candidate == wantZip {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("zip candidates = %#v, missing %q", info.ZipCandidates, wantZip)
	}
}

func TestDiscoverConnectorHTMLLinkToRecordingBase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mod/adobeconnect/joinrecording.php":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<script>window.open('/p456/?session=xyz')</script>`))
		case "/p456/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>recording</body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	j := &job{status: jobStatus{ID: "test", Logs: []string{}}}
	info := discover(server.URL+"/mod/adobeconnect/joinrecording.php?id=42", server.Client(), "", j)
	wantBase := server.URL + "/p456/?session=xyz"
	if len(info.RecordingBases) != 1 || info.RecordingBases[0] != wantBase {
		t.Fatalf("RecordingBases = %#v, want %q", info.RecordingBases, wantBase)
	}
}

func TestDiscoverDoesNotTreatLoginRedirectAsRecordingBase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mod/adobeconnect/joinrecording.php":
			http.Redirect(w, r, "/login/index.php", http.StatusFound)
		case "/login/index.php":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><script src="/lib/javascript.php/1772539765/lib/polyfills/polyfill.js"></script></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	j := &job{status: jobStatus{ID: "test", Logs: []string{}}}
	info := discover(server.URL+"/mod/adobeconnect/joinrecording.php?id=42", server.Client(), "", j)
	if info.LoginURL != server.URL+"/login/index.php" {
		t.Fatalf("LoginURL = %q, want login page", info.LoginURL)
	}
	if len(info.RecordingBases) != 0 {
		t.Fatalf("RecordingBases = %#v, want none", info.RecordingBases)
	}
}

func TestLooksLikeRecordingURLRejectsMoodleAssets(t *testing.T) {
	cases := []string{
		"https://binaloud.dpm.ir/login/index.php",
		"https://binaloud.dpm.ir/lib/javascript.php/1772539765/lib/polyfills/polyfill.js",
		"https://binaloud.dpm.ir/theme/styles.php/theme/boost",
	}
	for _, tc := range cases {
		if looksLikeRecordingURL(tc) {
			t.Fatalf("looksLikeRecordingURL(%q) = true, want false", tc)
		}
	}
	if !looksLikeRecordingURL("https://connect.example.edu/p123456/") {
		t.Fatal("real p-number recording URL was rejected")
	}
}

func TestDirectMediaFromSidecarXMLName(t *testing.T) {
	payload := []byte{0xff, 0xfb, 0x90, 0x64, 0x44, 0x55, 0x66}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p123/output/indexstream.xml":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<events><stream href="cameraVoip_1_11.xml"/></events>`))
		case "/p123/output/cameraVoip_1_11.flv":
			w.Header().Set("Content-Type", "video/x-flv")
			_, _ = w.Write(buildAudioFLV(2, payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	workDir := t.TempDir()
	j := &job{workDir: workDir, status: jobStatus{ID: "test", Logs: []string{}}}
	files, err := downloadDirectMedia(server.Client(), []string{server.URL + "/p123/"}, "", workDir, j)
	if err != nil {
		t.Fatalf("downloadDirectMedia returned error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	output := filepath.Join(workDir, "audio.mp3")
	result, err := extractFromFiles(files, workDir, output, j)
	if err != nil {
		t.Fatalf("extractFromFiles returned error: %v", err)
	}
	if result.Bytes != int64(len(payload)) {
		t.Fatalf("result.Bytes = %d, want %d", result.Bytes, len(payload))
	}
}

func TestDirectMediaCommonCameraVoipFallback(t *testing.T) {
	payload := []byte{0xff, 0xfb, 0x90, 0x64, 0x77, 0x88, 0x99}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p123/output/indexstream.xml":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<events><stream name="cameraVoip"/></events>`))
		case "/p123/output/cameraVoip_1_11.flv":
			w.Header().Set("Content-Type", "video/x-flv")
			_, _ = w.Write(buildAudioFLV(2, payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	workDir := t.TempDir()
	j := &job{workDir: workDir, status: jobStatus{ID: "test", Logs: []string{}}}
	files, err := downloadDirectMedia(server.Client(), []string{server.URL + "/p123/"}, "", workDir, j)
	if err != nil {
		t.Fatalf("downloadDirectMedia returned error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	output := filepath.Join(workDir, "audio.mp3")
	result, err := extractFromFiles(files, workDir, output, j)
	if err != nil {
		t.Fatalf("extractFromFiles returned error: %v", err)
	}
	if result.Bytes != int64(len(payload)) {
		t.Fatalf("result.Bytes = %d, want %d", result.Bytes, len(payload))
	}
}

func TestUniqueZipName(t *testing.T) {
	used := map[string]bool{}
	cases := []struct{ in, want string }{
		{"acgir-p1.mp3", "acgir-p1.mp3"},
		{"acgir-p1.mp3", "acgir-p1-2.mp3"},
		{"acgir-p1.mp3", "acgir-p1-3.mp3"},
		{"acgir-p2.mp3", "acgir-p2.mp3"},
	}
	for _, c := range cases {
		if got := uniqueZipName(c.in, used); got != c.want {
			t.Fatalf("uniqueZipName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSafeOutputNameDistinguishesConnectorLinks(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://connect.example.edu/p123456/", "acgir-p123456.mp3"},
		{"https://lms.example.ir/mod/adobeconnect/joinrecording.php?id=42&session=abc", "acgir-recording-42.mp3"},
		{"https://lms.example.ir/mod/adobeconnect/joinrecording.php?id=7", "acgir-recording-7.mp3"},
	}
	for _, c := range cases {
		if got := safeOutputName(c.in); got != c.want {
			t.Fatalf("safeOutputName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Two different recordings must not collide.
	a := safeOutputName("https://lms.example.ir/mod/adobeconnect/joinrecording.php?id=42")
	b := safeOutputName("https://lms.example.ir/mod/adobeconnect/joinrecording.php?id=43")
	if a == b {
		t.Fatalf("connector links with different ids collided: %q", a)
	}
}

func TestHandleDownloadAllBundlesReadyJobs(t *testing.T) {
	dir := t.TempDir()
	manager := newJobManager()

	add := func(id, filename string, payload []byte) {
		p := filepath.Join(dir, id+".mp3")
		if err := os.WriteFile(p, payload, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		j := &job{outputPath: p, status: jobStatus{ID: id, State: "done", Filename: filename, Logs: []string{}}}
		manager.jobs[id] = j
	}
	add("a", "acgir-rec.mp3", []byte("AAAA"))
	add("b", "acgir-rec.mp3", []byte("BBBB")) // same filename -> must be deduped in the zip
	// An unfinished job should be skipped.
	manager.jobs["c"] = &job{status: jobStatus{ID: "c", State: "running", Logs: []string{}}}

	req := httptest.NewRequest(http.MethodGet, "/api/download-all?ids=a,b,c,missing", nil)
	rec := httptest.NewRecorder()
	manager.handleDownloadAll(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("zip has %d files, want 2", len(zr.File))
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["acgir-rec.mp3"] || !names["acgir-rec-2.mp3"] {
		t.Fatalf("zip names = %v, want acgir-rec.mp3 and acgir-rec-2.mp3", names)
	}
}

func buildAudioFLV(format byte, payload []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{'F', 'L', 'V', 0x01, 0x04})
	_ = binary.Write(&b, binary.BigEndian, uint32(9))
	_ = binary.Write(&b, binary.BigEndian, uint32(0))

	dataSize := 1 + len(payload)
	b.WriteByte(8)
	writeU24(&b, dataSize)
	writeU24(&b, 0)
	b.WriteByte(0)
	writeU24(&b, 0)
	b.WriteByte(format<<4 | 0x0f)
	b.Write(payload)
	_ = binary.Write(&b, binary.BigEndian, uint32(11+dataSize))
	return b.Bytes()
}

func writeU24(b *bytes.Buffer, n int) {
	b.WriteByte(byte(n >> 16))
	b.WriteByte(byte(n >> 8))
	b.WriteByte(byte(n))
}

func buildRecordingZip(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("zip Create: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("zip Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return b.Bytes()
}
