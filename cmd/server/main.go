package main

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	pastebox "pastebox/internal"
)

type app struct {
	store *pastebox.Store
	index *template.Template
}

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	dataDir := getenv("DATA_DIR", "/paste-data")
	expireDays := getenvInt("EXPIRE_DAYS", 30)

	store, err := pastebox.NewStore(dataDir, time.Duration(expireDays)*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to initialize store: %v", err)
	}

	indexTemplate, err := template.ParseFiles("templates/index.html")
	if err != nil {
		indexTemplate = template.Must(template.New("index").Parse(fallbackIndexHTML))
	}

	a := &app{
		store: store,
		index: indexTemplate,
	}

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			if err := store.CleanupExpired(); err != nil {
				log.Printf("cleanup failed: %v", err)
			}
			<-ticker.C
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handle)

	log.Printf("pastebox listening on %s, data=%s", listenAddr, dataDir)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func (a *app) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/admin") {
		a.adminHandler(w, r)
		return
	}

	if r.URL.Path == "/" {
		switch r.Method {
		case http.MethodGet:
			a.indexHandler(w, r)
		case http.MethodPost, http.MethodPut:
			a.uploadHandler(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(id, "/") || id == "" {
		http.NotFound(w, r)
		return
	}

	if token := r.URL.Query().Get("delete"); token != "" {
		a.deleteHandler(w, r, id, token)
		return
	}

	a.viewHandler(w, r, id)
}

func (a *app) indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := map[string]string{
		"BaseURL": requestBaseURL(r),
	}

	if err := a.index.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (a *app) uploadHandler(w http.ResponseWriter, r *http.Request) {
	var reader io.Reader
	var filename string
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file field", http.StatusBadRequest)
			return
		}
		defer file.Close()

		reader = file

		if header != nil {
			filename = header.Filename
			if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(header.Filename))); detected != "" {
				contentType = detected
			} else {
				contentType = "application/octet-stream"
			}
		}
	} else {
		reader = r.Body
		if strings.TrimSpace(contentType) == "" {
			contentType = "text/plain; charset=utf-8"
		}
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, "failed to read upload", http.StatusBadRequest)
		return
	}

	allowed, reason := allowTextUpload(filename, contentType, content)
	if !allowed {
		log.Printf("upload blocked: remote=%s filename=%q content_type=%q reason=%s", r.RemoteAddr, filename, contentType, reason)
		http.Error(w, "unsupported file type. only text-based files are allowed", http.StatusUnsupportedMediaType)
		return
	}

	contentType = normalizeTextContentType(filename, contentType)

	usePassword := strings.EqualFold(strings.TrimSpace(r.Header.Get("usepassword")), "true")
	permanent := strings.EqualFold(strings.TrimSpace(r.Header.Get("data-policy")), "permanent")

	meta, password, deleteToken, err := a.store.Create(bytes.NewReader(content), contentType, usePassword, permanent)
	if err != nil {
		log.Printf("upload failed: %v", err)
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}

	url := strings.TrimRight(requestBaseURL(r), "/") + "/" + meta.ID

	log.Printf(
		"created: id=%s remote=%s size=%d content_type=%q policy=%s expires=%s protected=%t",
		meta.ID,
		r.RemoteAddr,
		meta.Size,
		meta.ContentType,
		meta.DataPolicy,
		formatExpiresForLog(meta),
		password != "",
	)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	fmt.Fprintf(w, "url: %s\n", url)

	if !strings.EqualFold(meta.DataPolicy, "permanent") && !meta.ExpiresAt.IsZero() {
		fmt.Fprintf(w, "expires: %s\n", meta.ExpiresAt.Format(time.RFC3339))
	}

	if password != "" {
		fmt.Fprintf(w, "password: %s\n", password)
	}

	fmt.Fprintf(w, "delete: %s?delete=%s\n", url, deleteToken)
}

func (a *app) deleteHandler(w http.ResponseWriter, r *http.Request, id string, token string) {
	if r.Method == http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := a.store.Delete(id, token); err != nil {
		if errors.Is(err, pastebox.ErrInvalidDeleteToken) {
			log.Printf("delete denied: id=%s remote=%s", id, r.RemoteAddr)
			http.Error(w, "delete token required or invalid", http.StatusUnauthorized)
			return
		}

		http.NotFound(w, r)
		return
	}

	log.Printf("deleted: id=%s remote=%s", id, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "deleted")
}

func (a *app) viewHandler(w http.ResponseWriter, r *http.Request, id string) {
	password := r.URL.Query().Get("password")
	if password == "" {
		password = r.Header.Get("paste-password")
	}

	entry, err := a.store.Open(id, password)
	if err != nil {
		if errors.Is(err, pastebox.ErrInvalidPassword) {
			http.Error(w, "password required or invalid. use ?password=... or paste-password header", http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
		return
	}
	defer entry.File.Close()

	raw := r.URL.Query().Get("raw") == "1"
	browser := isBrowserRequest(r)

	if !raw && browser && isTextEntry(entry) {
		content, err := io.ReadAll(entry.File)
		if err != nil {
			http.Error(w, "failed to read file", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		_ = pasteViewHTML.Execute(w, map[string]any{
			"ID":      entry.Meta.ID,
			"Content": string(content),
		})
		return
	}

	contentType := entry.Meta.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if isTextEntry(entry) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, entry.Meta.ID))
	}

	if r.Method == http.MethodHead {
		return
	}

	_, _ = io.Copy(w, entry.File)
}

func (a *app) adminHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/admin", "/admin/":
		a.adminIndexHandler(w, r)
	case "/admin/setup":
		a.adminSetupHandler(w, r)
	case "/admin/login":
		a.adminLoginHandler(w, r)
	case "/admin/logout":
		a.adminLogoutHandler(w, r)
	case "/admin/delete":
		a.adminDeleteHandler(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (a *app) adminIndexHandler(w http.ResponseWriter, r *http.Request) {
	exists, err := a.store.AdminExists()
	if err != nil {
		http.Error(w, "admin database error", http.StatusInternalServerError)
		return
	}

	if !exists {
		http.Redirect(w, r, "/admin/setup", http.StatusSeeOther)
		return
	}

	if !a.requireAdmin(w, r) {
		return
	}

	items, err := a.store.ListPastes()
	if err != nil {
		http.Error(w, "failed to list pastes", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Items":   items,
		"Stats":   buildAdminStats(items),
		"BaseURL": requestBaseURL(r),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminListHTML.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

type adminStats struct {
	Total          int
	Temporary      int
	Permanent      int
	Protected      int
	Expiring24h    int
	Expired        int
	TotalSizeBytes int64
	TotalSize      string
}

func buildAdminStats(items []pastebox.AdminPasteItem) adminStats {
	now := time.Now().UTC()
	stats := adminStats{
		Total: len(items),
	}

	for _, item := range items {
		stats.TotalSizeBytes += item.Size

		if strings.EqualFold(item.DataPolicy, "permanent") {
			stats.Permanent++
		} else {
			stats.Temporary++
		}

		if item.Protected {
			stats.Protected++
		}

		if !item.ExpiresAt.IsZero() {
			if now.After(item.ExpiresAt) {
				stats.Expired++
			} else if item.ExpiresAt.Sub(now) <= 24*time.Hour {
				stats.Expiring24h++
			}
		}
	}

	stats.TotalSize = formatBytes(stats.TotalSizeBytes)
	return stats
}

func formatBytes(size int64) string {
	const unit = 1024

	if size < unit {
		return fmt.Sprintf("%d B", size)
	}

	div := int64(unit)
	exp := 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func (a *app) adminSetupHandler(w http.ResponseWriter, r *http.Request) {
	exists, err := a.store.AdminExists()
	if err != nil {
		http.Error(w, "admin database error", http.StatusInternalServerError)
		return
	}

	if exists {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}

	if r.Method == http.MethodGet {
		a.renderAdminForm(w, "Create admin", "/admin/setup", "", "Create")
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderAdminForm(w, "Create admin", "/admin/setup", "Invalid form", "Create")
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	if err := a.store.CreateAdmin(username, password); err != nil {
		a.renderAdminForm(w, "Create admin", "/admin/setup", err.Error(), "Create")
		return
	}

	log.Printf("admin created: username=%s remote=%s", username, r.RemoteAddr)

	token, err := a.store.CreateAdminSession()
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	setAdminCookie(w, token)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *app) adminLoginHandler(w http.ResponseWriter, r *http.Request) {
	exists, err := a.store.AdminExists()
	if err != nil {
		http.Error(w, "admin database error", http.StatusInternalServerError)
		return
	}

	if !exists {
		http.Redirect(w, r, "/admin/setup", http.StatusSeeOther)
		return
	}

	if r.Method == http.MethodGet {
		a.renderAdminForm(w, "Admin login", "/admin/login", "", "Login")
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderAdminForm(w, "Admin login", "/admin/login", "Invalid form", "Login")
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	ok, err := a.store.AuthenticateAdmin(username, password)
	if err != nil {
		http.Error(w, "admin database error", http.StatusInternalServerError)
		return
	}

	if !ok {
		log.Printf("admin login failed: username=%s remote=%s", username, r.RemoteAddr)
		a.renderAdminForm(w, "Admin login", "/admin/login", "Invalid username or password", "Login")
		return
	}

	token, err := a.store.CreateAdminSession()
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	log.Printf("admin login: username=%s remote=%s", username, r.RemoteAddr)

	setAdminCookie(w, token)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *app) adminLogoutHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("pastebox_admin")
	if err == nil {
		_ = a.store.DeleteAdminSession(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "pastebox_admin",
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (a *app) adminDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	id := r.FormValue("id")
	if err := a.store.AdminDelete(id); err != nil {
		http.Error(w, "delete failed", http.StatusBadRequest)
		return
	}

	log.Printf("admin deleted: id=%s remote=%s", id, r.RemoteAddr)

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *app) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	cookie, err := r.Cookie("pastebox_admin")
	if err != nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return false
	}

	ok, err := a.store.ValidAdminSession(cookie.Value)
	if err != nil || !ok {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return false
	}

	return true
}

func (a *app) renderAdminForm(w http.ResponseWriter, title string, action string, errorMessage string, button string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	_ = adminFormHTML.Execute(w, map[string]any{
		"Title": title,
		"Action": action,
		"Error": errorMessage,
		"Button": button,
	})
}

func setAdminCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "pastebox_admin",
		Value:    token,
		Path:     "/admin",
		MaxAge:   86400,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func allowTextUpload(filename string, contentType string, content []byte) (bool, string) {
	ext := normalizedUploadExt(filename)
	lowerContentType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))

	if isBlockedUploadExtension(ext) {
		return false, "blocked extension"
	}

	if isBlockedUploadContentType(lowerContentType) {
		return false, "blocked content type"
	}

	if isKnownTextExtension(ext) {
		if looksLikeText(content) {
			return true, ""
		}
		return false, "text extension but binary content"
	}

	if isTextContentType(lowerContentType) {
		if looksLikeText(content) {
			return true, ""
		}
		return false, "text content type but binary content"
	}

	if looksLikeText(content) {
		return true, ""
	}

	return false, "not text"
}

func normalizeTextContentType(filename string, contentType string) string {
	ext := normalizedUploadExt(filename)
	lowerContentType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))

	if lowerContentType != "" && isTextContentType(lowerContentType) {
		if strings.Contains(strings.ToLower(contentType), "charset=") {
			return contentType
		}
		return lowerContentType + "; charset=utf-8"
	}

	switch ext {
	case ".csv", ".tsv":
		return "text/csv; charset=utf-8"
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".json", ".jsonl":
		return "application/json; charset=utf-8"
	case ".xml":
		return "application/xml; charset=utf-8"
	case ".yaml", ".yml":
		return "application/yaml; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs", ".cjs":
		return "application/javascript; charset=utf-8"
	case ".sh", ".bash", ".zsh":
		return "text/x-shellscript; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}

func normalizedUploadExt(filename string) string {
	name := strings.ToLower(strings.TrimSpace(filename))
	if name == "" {
		return ""
	}

	if strings.HasSuffix(name, ".tar.gz") {
		return ".tar.gz"
	}
	if strings.HasSuffix(name, ".tar.xz") {
		return ".tar.xz"
	}
	if strings.HasSuffix(name, ".tar.bz2") {
		return ".tar.bz2"
	}

	return filepath.Ext(name)
}

func isKnownTextExtension(ext string) bool {
	switch ext {
	case "",
		".txt", ".text", ".log", ".md", ".markdown", ".csv", ".tsv",
		".json", ".jsonl", ".xml", ".yaml", ".yml", ".toml", ".ini", ".env",
		".conf", ".cfg", ".properties", ".sql",
		".html", ".htm", ".css", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx",
		".go", ".py", ".rb", ".php", ".java", ".kt", ".kts", ".c", ".h", ".cpp", ".hpp",
		".rs", ".swift", ".cs", ".sh", ".bash", ".zsh", ".fish", ".ps1", ".bat",
		".dockerfile", ".gitignore", ".gitattributes", ".editorconfig":
		return true
	default:
		return false
	}
}

func isBlockedUploadExtension(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".bmp", ".svg", ".gif", ".webp", ".ico", ".tif", ".tiff",
		".mp4", ".mp3", ".mpv", ".mkv", ".mov", ".avi", ".wmv", ".flv", ".webm", ".m4v",
		".wav", ".flac", ".aac", ".ogg", ".m4a",
		".iso", ".zip", ".tar", ".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".tbz2",
		".gz", ".xz", ".bz2", ".7z", ".rar",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".exe", ".dll", ".so", ".dylib", ".bin", ".img", ".apk", ".deb", ".rpm":
		return true
	default:
		return false
	}
}

func isBlockedUploadContentType(contentType string) bool {
	if contentType == "" {
		return false
	}

	if strings.HasPrefix(contentType, "image/") ||
		strings.HasPrefix(contentType, "video/") ||
		strings.HasPrefix(contentType, "audio/") {
		return true
	}

	switch contentType {
	case "application/zip",
		"application/x-zip-compressed",
		"application/x-tar",
		"application/gzip",
		"application/x-gzip",
		"application/x-7z-compressed",
		"application/vnd.rar",
		"application/x-rar-compressed",
		"application/x-iso9660-image",
		"application/pdf",
		"application/octet-stream":
		return true
	default:
		return false
	}
}

func isTextContentType(contentType string) bool {
	if contentType == "" {
		return false
	}

	if strings.HasPrefix(contentType, "text/") {
		return true
	}

	if strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "xml") ||
		strings.Contains(contentType, "yaml") ||
		strings.Contains(contentType, "toml") ||
		strings.Contains(contentType, "javascript") ||
		strings.Contains(contentType, "ecmascript") ||
		strings.Contains(contentType, "x-sh") ||
		strings.Contains(contentType, "x-shellscript") {
		return true
	}

	return false
}

func isBrowserRequest(r *http.Request) bool {
	ua := strings.ToLower(r.UserAgent())
	if strings.HasPrefix(ua, "curl/") || strings.Contains(ua, "wget/") || strings.Contains(ua, "httpie/") {
		return false
	}

	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "text/html") || accept == ""
}

func isTextEntry(entry *pastebox.Entry) bool {
	contentType := strings.ToLower(entry.Meta.ContentType)
	if strings.HasPrefix(contentType, "text/") {
		return true
	}

	if strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "xml") ||
		strings.Contains(contentType, "yaml") ||
		strings.Contains(contentType, "javascript") ||
		strings.Contains(contentType, "x-sh") {
		return true
	}

	pos, _ := entry.File.Seek(0, io.SeekCurrent)

	buf := make([]byte, 4096)
	n, _ := entry.File.Read(buf)
	_, _ = entry.File.Seek(pos, io.SeekStart)

	return looksLikeText(buf[:n])
}

func looksLikeText(buf []byte) bool {
	if len(buf) == 0 {
		return true
	}

	if bytes.IndexByte(buf, 0) >= 0 {
		return false
	}

	if !utf8.Valid(buf) {
		return false
	}

	bad := 0
	for _, b := range buf {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			bad++
		}
	}

	return bad == 0
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	host := r.Host

	if r.TLS != nil {
		scheme = "https"
	}

	if forwarded := r.Header.Get("Forwarded"); forwarded != "" {
		parts := strings.Split(forwarded, ";")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToLower(part), "proto=") {
				scheme = strings.Trim(strings.TrimPrefix(part, "proto="), `"`)
			}
			if strings.HasPrefix(strings.ToLower(part), "host=") {
				host = strings.Trim(strings.TrimPrefix(part, "host="), `"`)
			}
		}
	}

	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.Split(proto, ",")[0]
		scheme = strings.TrimSpace(scheme)
	}

	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		host = strings.Split(forwardedHost, ",")[0]
		host = strings.TrimSpace(host)
	}

	if host == "" {
		host = "localhost"
	}

	return scheme + "://" + host
}

func formatExpiresForLog(meta pastebox.Metadata) string {
	if strings.EqualFold(meta.DataPolicy, "permanent") || meta.ExpiresAt.IsZero() {
		return "-"
	}

	return meta.ExpiresAt.Format(time.RFC3339)
}

func getenv(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}

	return n
}

var pasteViewHTML = template.Must(template.New("paste").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{ .ID }} - Pastebox</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-[#111111] text-zinc-100">
  <header class="sticky top-0 z-10 border-b border-white/10 bg-[#111111]/95 backdrop-blur">
    <div class="mx-auto flex max-w-screen-2xl items-center justify-between gap-4 px-6 py-4">
      <div class="min-w-0">
        <p class="text-xs uppercase tracking-[0.24em] text-zinc-500">Pastebox</p>
        <h1 class="truncate font-mono text-lg font-semibold text-zinc-100">{{ .ID }}</h1>
      </div>

      <div class="flex shrink-0 items-center gap-2">
        <button
          id="copyButton"
          type="button"
          class="rounded-lg border border-white/10 px-3 py-2 text-sm text-zinc-300 transition hover:border-white/20 hover:bg-white/[0.04] hover:text-white"
          onclick="copyPasteContent()"
        >
          Copy
        </button>
        <a
          class="rounded-lg border border-white/10 px-3 py-2 text-sm text-zinc-300 transition hover:border-white/20 hover:bg-white/[0.04] hover:text-white"
          href="?raw=1"
        >
          Raw
        </a>
      </div>
    </div>
  </header>

  <main class="mx-auto max-w-screen-2xl px-6 py-6">
    <pre id="pasteContent" class="min-h-[calc(100vh-8rem)] overflow-x-auto whitespace-pre-wrap break-words font-mono text-sm leading-6 text-zinc-200">{{ .Content }}</pre>
  </main>

  <script>
    async function copyPasteContent() {
      const button = document.getElementById("copyButton");
      const content = document.getElementById("pasteContent").innerText;

      try {
        await navigator.clipboard.writeText(content);
        button.innerText = "Copied";
      } catch (error) {
        const textarea = document.createElement("textarea");
        textarea.value = content;
        textarea.setAttribute("readonly", "");
        textarea.style.position = "fixed";
        textarea.style.left = "-9999px";
        document.body.appendChild(textarea);
        textarea.select();
        document.execCommand("copy");
        document.body.removeChild(textarea);
        button.innerText = "Copied";
      }

      setTimeout(() => {
        button.innerText = "Copy";
      }, 1500);
    }
  </script>
</body>
</html>`))

var adminFormHTML = template.Must(template.New("admin-form").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{ .Title }} - Pastebox</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-[#111111] text-zinc-100">
  <main class="mx-auto flex min-h-screen max-w-md flex-col justify-center px-6 py-12">
    <section class="rounded-2xl border border-white/10 bg-[#151515]/80 p-8 shadow-xl transition-all duration-300 ease-out hover:border-white/20 hover:bg-[#161616]/90 hover:shadow-[0_0_28px_rgba(255,255,255,0.06)]">
      <p class="mb-3 inline-flex rounded-full border border-white/10 px-3 py-1 text-xs text-zinc-400">Pastebox Admin</p>
      <h1 class="text-3xl font-bold tracking-tight text-white">{{ .Title }}</h1>
      <p class="mt-3 text-sm text-zinc-400">The first account becomes the only administrator account.</p>

      {{ if .Error }}
      <div class="mt-5 rounded-xl border border-red-400/20 bg-red-500/10 p-3 text-sm text-red-200">{{ .Error }}</div>
      {{ end }}

      <form class="mt-6 space-y-4" method="post" action="{{ .Action }}">
        <div>
          <label class="mb-2 block text-sm text-zinc-400">Username</label>
          <input class="w-full rounded-xl border border-white/10 bg-black/30 px-4 py-3 text-zinc-100 outline-none transition focus:border-white/30" name="username" autocomplete="username" required>
        </div>
        <div>
          <label class="mb-2 block text-sm text-zinc-400">Password</label>
          <input class="w-full rounded-xl border border-white/10 bg-black/30 px-4 py-3 text-zinc-100 outline-none transition focus:border-white/30" name="password" type="password" autocomplete="current-password" required>
        </div>
        <button class="w-full rounded-xl bg-zinc-100 px-4 py-3 font-semibold text-zinc-950 transition hover:bg-white" type="submit">{{ .Button }}</button>
      </form>
    </section>
  </main>
</body>
</html>`))

var adminListHTML = template.Must(template.New("admin-list").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Admin - Pastebox</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-[#111111] text-zinc-100">
  <main class="mx-auto min-h-screen max-w-6xl px-6 py-12">
    <section class="rounded-2xl border border-white/10 bg-[#151515]/80 p-8 shadow-xl transition-all duration-300 ease-out hover:border-white/20 hover:bg-[#161616]/90 hover:shadow-[0_0_28px_rgba(255,255,255,0.06)]">
      <div class="mb-8 flex flex-col gap-4 md:flex-row md:items-end md:justify-between">
        <div>
          <p class="mb-3 inline-flex rounded-full border border-white/10 px-3 py-1 text-xs text-zinc-400">Pastebox Admin</p>
          <h1 class="text-4xl font-bold tracking-tight text-white">Links</h1>
          <p class="mt-3 text-sm text-zinc-400">Manage currently stored local paste files.</p>
        </div>
        <div class="flex gap-3">
          <a class="rounded-xl border border-white/10 px-4 py-2 text-zinc-300 transition hover:border-white/20 hover:text-white" href="/">Home</a>
          <a class="rounded-xl border border-white/10 px-4 py-2 text-zinc-300 transition hover:border-white/20 hover:text-white" href="/admin/logout">Logout</a>
        </div>
      </div>

      <div class="mb-8 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <div class="rounded-2xl border border-white/10 bg-black/25 p-5 transition-all duration-300 hover:border-white/20 hover:bg-white/[0.03] hover:shadow-[0_0_24px_rgba(255,255,255,0.05)]">
          <p class="text-xs uppercase tracking-[0.18em] text-zinc-500">Total Links</p>
          <p class="mt-3 text-3xl font-bold text-white">{{ .Stats.Total }}</p>
          <p class="mt-1 text-sm text-zinc-500">stored pastes</p>
        </div>
        <div class="rounded-2xl border border-white/10 bg-black/25 p-5 transition-all duration-300 hover:border-white/20 hover:bg-white/[0.03] hover:shadow-[0_0_24px_rgba(255,255,255,0.05)]">
          <p class="text-xs uppercase tracking-[0.18em] text-zinc-500">Storage</p>
          <p class="mt-3 text-3xl font-bold text-white">{{ .Stats.TotalSize }}</p>
          <p class="mt-1 text-sm text-zinc-500">total size</p>
        </div>
        <div class="rounded-2xl border border-white/10 bg-black/25 p-5 transition-all duration-300 hover:border-white/20 hover:bg-white/[0.03] hover:shadow-[0_0_24px_rgba(255,255,255,0.05)]">
          <p class="text-xs uppercase tracking-[0.18em] text-zinc-500">Policy</p>
          <p class="mt-3 text-3xl font-bold text-white">{{ .Stats.Temporary }} / {{ .Stats.Permanent }}</p>
          <p class="mt-1 text-sm text-zinc-500">temporary / permanent</p>
        </div>
        <div class="rounded-2xl border border-white/10 bg-black/25 p-5 transition-all duration-300 hover:border-white/20 hover:bg-white/[0.03] hover:shadow-[0_0_24px_rgba(255,255,255,0.05)]">
          <p class="text-xs uppercase tracking-[0.18em] text-zinc-500">Security</p>
          <p class="mt-3 text-3xl font-bold text-white">{{ .Stats.Protected }}</p>
          <p class="mt-1 text-sm text-zinc-500">password-protected</p>
        </div>
      </div>

      <div class="mb-8 grid gap-4 sm:grid-cols-2">
        <div class="rounded-2xl border border-yellow-400/10 bg-yellow-500/[0.04] p-5">
          <p class="text-xs uppercase tracking-[0.18em] text-yellow-200/60">Expiring Soon</p>
          <p class="mt-3 text-2xl font-bold text-yellow-100">{{ .Stats.Expiring24h }}</p>
          <p class="mt-1 text-sm text-yellow-100/50">temporary links expiring within 24 hours</p>
        </div>
        <div class="rounded-2xl border border-red-400/10 bg-red-500/[0.04] p-5">
          <p class="text-xs uppercase tracking-[0.18em] text-red-200/60">Expired</p>
          <p class="mt-3 text-2xl font-bold text-red-100">{{ .Stats.Expired }}</p>
          <p class="mt-1 text-sm text-red-100/50">links waiting for cleanup</p>
        </div>
      </div>

      <div class="overflow-x-auto rounded-2xl border border-white/10">
        <table class="min-w-full divide-y divide-white/10 text-sm">
          <thead class="bg-black/30 text-left text-xs uppercase tracking-wider text-zinc-500">
            <tr>
              <th class="px-4 py-3">Code</th>
              <th class="px-4 py-3">Policy</th>
              <th class="px-4 py-3">Size</th>
              <th class="px-4 py-3">Protected</th>
              <th class="px-4 py-3">Created</th>
              <th class="px-4 py-3">Expires</th>
              <th class="px-4 py-3">Action</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-white/10">
            {{ range .Items }}
            <tr class="transition hover:bg-white/[0.03]">
              <td class="whitespace-nowrap px-4 py-3">
                <a class="font-mono text-zinc-100 underline decoration-zinc-700 underline-offset-4 hover:decoration-zinc-100" href="{{ $.BaseURL }}/{{ .ID }}" target="_blank">{{ .ID }}</a>
              </td>
              <td class="whitespace-nowrap px-4 py-3 text-zinc-300">{{ .DataPolicy }}</td>
              <td class="whitespace-nowrap px-4 py-3 text-zinc-300">{{ .Size }}</td>
              <td class="whitespace-nowrap px-4 py-3 text-zinc-300">{{ .Protected }}</td>
              <td class="whitespace-nowrap px-4 py-3 text-zinc-400">{{ .CreatedAt.Format "2006-01-02 15:04:05" }}</td>
              <td class="whitespace-nowrap px-4 py-3 text-zinc-400">{{ if .ExpiresAt.IsZero }}-{{ else }}{{ .ExpiresAt.Format "2006-01-02 15:04:05" }}{{ end }}</td>
              <td class="whitespace-nowrap px-4 py-3">
                <form method="post" action="/admin/delete" onsubmit="return confirm('Delete {{ .ID }}?')">
                  <input type="hidden" name="id" value="{{ .ID }}">
                  <button class="rounded-lg border border-red-400/20 px-3 py-1.5 text-red-200 transition hover:bg-red-500/10" type="submit">Delete</button>
                </form>
              </td>
            </tr>
            {{ else }}
            <tr>
              <td class="px-4 py-8 text-center text-zinc-500" colspan="7">No pastes found.</td>
            </tr>
            {{ end }}
          </tbody>
        </table>
      </div>
    </section>
  </main>
</body>
</html>`))

const fallbackIndexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Pastebox</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-[#111111] text-gray-200">
  <main class="mx-auto flex min-h-screen max-w-3xl flex-col justify-center px-6">
    <div class="rounded-2xl border border-gray-800 bg-[#151515] p-8 shadow-2xl">
      <h1 class="text-3xl font-bold text-white">Pastebox</h1>
      <p class="mt-3 text-gray-400">curl-based file sharing service</p>
    </div>
  </main>
</body>
</html>`
