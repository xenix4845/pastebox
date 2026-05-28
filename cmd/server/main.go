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
	store        *pastebox.Store
	index        *template.Template
	paste        *template.Template
	adminForm    *template.Template
	adminList    *template.Template
	passwordPage *template.Template
	notFoundPage *template.Template
	cloneResult  *template.Template
}

const maxUploadSize int64 = 1 << 30 // 1 GiB
const uploadSampleSize int64 = 64 << 10 // 64 KiB

func main() {
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	dataDir := getenv("DATA_DIR", "/paste-data")
	expireDays := getenvInt("EXPIRE_DAYS", 30)

	store, err := pastebox.NewStore(dataDir, time.Duration(expireDays)*24*time.Hour)
	if err != nil {
		log.Fatalf("failed to initialize store: %v", err)
	}

	a := &app{
		store:        store,
		index:        mustParseTemplate("templates/index.html"),
		paste:        mustParseTemplate("templates/paste.html"),
		adminForm:    mustParseTemplate("templates/admin_form.html"),
		adminList:    mustParseTemplate("templates/admin_list.html"),
		passwordPage: mustParseTemplate("templates/password.html"),
		notFoundPage: mustParseTemplate("templates/404.html"),
		cloneResult:  mustParseTemplate("templates/clone.html"),
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

	id := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(id, "/") || id == "" {
		a.notFoundHandler(w, r)
		return
	}

	if r.Method == http.MethodPost {
		a.cloneHandler(w, r, id)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	disabled, err := a.store.UploadsDisabled()
	if err != nil {
		log.Printf("failed to read upload status: %v", err)
		http.Error(w, "upload status unavailable", http.StatusInternalServerError)
		return
	}

	if disabled {
		http.Error(w, "new uploads are currently disabled", http.StatusServiceUnavailable)
		return
	}

	var reader io.Reader
	var filename string
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
				http.Error(w, "upload too large. maximum size is 1GB", http.StatusRequestEntityTooLarge)
				return
			}

			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}

		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
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

			if header.Size > maxUploadSize {
				http.Error(w, "upload too large. maximum size is 1GB", http.StatusRequestEntityTooLarge)
				return
			}

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

	reader, allowed, reason, err := prepareTextUploadReader(filename, contentType, reader)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
			http.Error(w, "upload too large. maximum size is 1GB", http.StatusRequestEntityTooLarge)
			return
		}

		http.Error(w, "failed to read upload", http.StatusBadRequest)
		return
	}

	if !allowed {
		log.Printf("upload blocked: remote=%s filename=%q content_type=%q reason=%s", r.RemoteAddr, filename, contentType, reason)
		http.Error(w, "unsupported file type. only text-based files are allowed", http.StatusUnsupportedMediaType)
		return
	}

	contentType = normalizeTextContentType(filename, contentType)

	usePassword := strings.EqualFold(strings.TrimSpace(r.Header.Get("usepassword")), "true")
	policy := strings.ToLower(strings.TrimSpace(r.Header.Get("data-policy")))
	permanent := policy == "permanent"
	once := policy == "once"
	customCode := strings.TrimSpace(r.Header.Get("code"))

	meta, password, deleteToken, err := a.store.Create(reader, contentType, usePassword, permanent, once, customCode)
	if err != nil {
		log.Printf("upload failed: %v", err)

		if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
			http.Error(w, "upload too large. maximum size is 1GB", http.StatusRequestEntityTooLarge)
			return
		}

		if errors.Is(err, pastebox.ErrInvalidCode) {
			http.Error(w, "invalid code. use 1-10 characters: letters, numbers, underscore, or hyphen", http.StatusBadRequest)
			return
		}

		if errors.Is(err, pastebox.ErrCodeExists) {
			http.Error(w, "code already exists", http.StatusConflict)
			return
		}

		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}

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

	a.writeUploadResponse(w, r, meta, password, deleteToken)
}

func (a *app) writeUploadResponse(w http.ResponseWriter, r *http.Request, meta pastebox.Metadata, password string, deleteToken string) {
	a.writeUploadResponseWithMode(w, r, meta, password, deleteToken, false)
}

func (a *app) writeCloneResponse(w http.ResponseWriter, r *http.Request, meta pastebox.Metadata, password string, deleteToken string) {
	a.writeUploadResponseWithMode(w, r, meta, password, deleteToken, true)
}

func (a *app) writeUploadResponseWithMode(w http.ResponseWriter, r *http.Request, meta pastebox.Metadata, password string, deleteToken string, clone bool) {
	url := strings.TrimRight(requestBaseURL(r), "/") + "/" + meta.ID
	deleteURL := url + "?delete=" + deleteToken

	expires := ""
	if !strings.EqualFold(meta.DataPolicy, "permanent") && !meta.ExpiresAt.IsZero() {
		expires = meta.ExpiresAt.Format(time.RFC3339)
	}

	if clone && isBrowserRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		_ = a.cloneResult.Execute(w, map[string]any{
			"URL":       url,
			"Expires":   expires,
			"Password":  password,
			"DeleteURL": deleteURL,
		})
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "url: %s\n", url)

	if expires != "" {
		fmt.Fprintf(w, "expires: %s\n", expires)
	}

	if password != "" {
		fmt.Fprintf(w, "password: %s\n", password)
	}

	fmt.Fprintf(w, "delete: %s\n", deleteURL)
}

func (a *app) cloneHandler(w http.ResponseWriter, r *http.Request, id string) {
	disabled, err := a.store.UploadsDisabled()
	if err != nil {
		log.Printf("failed to read upload status: %v", err)
		http.Error(w, "upload status unavailable", http.StatusInternalServerError)
		return
	}

	if disabled {
		http.Error(w, "new uploads are currently disabled", http.StatusServiceUnavailable)
		return
	}

	password := r.FormValue("password")
	if password == "" {
		password = r.URL.Query().Get("password")
	}
	if password == "" {
		password = r.Header.Get("paste-password")
	}

	usePassword := strings.EqualFold(strings.TrimSpace(r.Header.Get("usepassword")), "true")
	policy := strings.ToLower(strings.TrimSpace(r.Header.Get("data-policy")))
	permanent := policy == "permanent"
	once := policy == "once"
	customCode := strings.TrimSpace(r.Header.Get("code"))

	meta, newPassword, deleteToken, err := a.store.Clone(id, password, usePassword, permanent, once, customCode)
	if err != nil {
		if errors.Is(err, pastebox.ErrInvalidPassword) {
			http.Error(w, "password required or invalid. use ?password=... or paste-password header", http.StatusUnauthorized)
			return
		}

		if errors.Is(err, pastebox.ErrInvalidCode) {
			http.Error(w, "invalid code. use 1-10 characters: letters, numbers, underscore, or hyphen", http.StatusBadRequest)
			return
		}

		if errors.Is(err, pastebox.ErrCodeExists) {
			http.Error(w, "code already exists", http.StatusConflict)
			return
		}

		http.NotFound(w, r)
		return
	}

	log.Printf(
		"cloned: source=%s id=%s remote=%s size=%d content_type=%q policy=%s expires=%s protected=%t",
	    id,
	    meta.ID,
	    r.RemoteAddr,
	    meta.Size,
	    meta.ContentType,
	    meta.DataPolicy,
	    formatExpiresForLog(meta),
		    newPassword != "",
	)

	a.writeCloneResponse(w, r, meta, newPassword, deleteToken)
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
			a.passwordRequiredHandler(w, r, id)
			return
		}
		a.notFoundHandler(w, r)
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

		_ = a.paste.Execute(w, map[string]any{
			"ID":       entry.Meta.ID,
			"Content":  string(content),
				    "Language": syntaxLanguage(entry.Meta.ContentType),
				    "Password": password,
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

func (a *app) passwordRequiredHandler(w http.ResponseWriter, r *http.Request, id string) {
	if !isBrowserRequest(r) {
		http.Error(w, "password required or invalid. use ?password=... or paste-password header", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)

	_ = a.passwordPage.Execute(w, map[string]any{
		"ID":     id,
		"Action": "/" + id,
	})
}

func (a *app) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	if !isBrowserRequest(r) {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)

	_ = a.notFoundPage.Execute(w, map[string]any{
		"Path": r.URL.Path,
	})
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
		case "/admin/uploads":
			a.adminUploadsHandler(w, r)
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

	uploadsDisabled, err := a.store.UploadsDisabled()
	if err != nil {
		http.Error(w, "failed to read upload status", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Items":           items,
		"Stats":           buildAdminStats(items),
		"BaseURL":         requestBaseURL(r),
		"UploadsDisabled": uploadsDisabled,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.adminList.Execute(w, data); err != nil {
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

func (a *app) adminUploadsHandler(w http.ResponseWriter, r *http.Request) {
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

	disabled := r.FormValue("disabled") == "true"

	if err := a.store.SetUploadsDisabled(disabled); err != nil {
		http.Error(w, "failed to update upload status", http.StatusInternalServerError)
		return
	}

	log.Printf("admin upload status changed: disabled=%t remote=%s", disabled, r.RemoteAddr)

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
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

	_ = a.adminForm.Execute(w, map[string]any{
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

func prepareTextUploadReader(filename string, contentType string, reader io.Reader) (io.Reader, bool, string, error) {
	sample, err := io.ReadAll(io.LimitReader(reader, uploadSampleSize))
	if err != nil {
		return nil, false, "", err
	}

	allowed, reason := allowTextUpload(filename, contentType, sample)
	if !allowed {
		return nil, false, reason, nil
	}

	return io.MultiReader(bytes.NewReader(sample), reader), true, "", nil
}

func allowTextUpload(filename string, contentType string, content []byte) (bool, string) {
	ext := normalizedUploadExt(filename)
	lowerContentType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))

	if isBlockedUploadExtension(ext) {
		return false, "blocked extension"
	}

	if isKnownTextExtension(ext) {
		if looksLikeText(content) {
			return true, ""
		}
		return false, "text extension but binary content"
	}

	if isBlockedUploadContentType(lowerContentType) {
		return false, "blocked content type"
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

	switch ext {
		case ".log":
			return "text/x-log; charset=utf-8"
		case ".rs":
			return "text/x-rust; charset=utf-8"
		case ".go":
			return "text/x-go; charset=utf-8"
		case ".js", ".mjs", ".cjs":
			return "application/javascript; charset=utf-8"
		case ".py":
			return "text/x-python; charset=utf-8"
		case ".md", ".markdown":
			return "text/markdown; charset=utf-8"
		case ".ts", ".tsx":
			return "text/typescript; charset=utf-8"
		case ".php":
			return "application/x-httpd-php; charset=utf-8"
		case ".html", ".htm":
			return "text/html; charset=utf-8"
		case ".css":
			return "text/css; charset=utf-8"
		case ".csv", ".tsv":
			return "text/csv; charset=utf-8"
		case ".json", ".jsonl":
			return "application/json; charset=utf-8"
		case ".xml":
			return "application/xml; charset=utf-8"
		case ".yaml", ".yml":
			return "application/yaml; charset=utf-8"
		case ".sh", ".bash", ".zsh":
			return "text/x-shellscript; charset=utf-8"
	}

	if lowerContentType != "" && isTextContentType(lowerContentType) {
		if strings.Contains(strings.ToLower(contentType), "charset=") {
			return contentType
		}
		return lowerContentType + "; charset=utf-8"
	}

	return "text/plain; charset=utf-8"
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

func syntaxLanguage(contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))

	switch {
		case strings.Contains(contentType, "x-log"):
			return "logs"
		case strings.Contains(contentType, "x-rust"):
			return "rust"
		case strings.Contains(contentType, "x-go"):
			return "go"
		case strings.Contains(contentType, "javascript"):
			return "javascript"
		case strings.Contains(contentType, "x-python"):
			return "python"
		case strings.Contains(contentType, "markdown"):
			return "markdown"
		case strings.Contains(contentType, "typescript"):
			return "typescript"
		case strings.Contains(contentType, "php"):
			return "php"
		case contentType == "text/html":
			return "xml"
		case contentType == "text/css":
			return "css"
		default:
			return "plaintext"
	}
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

func mustParseTemplate(path string) *template.Template {
	tpl, err := template.ParseFiles(path)
	if err != nil {
		log.Fatalf("failed to parse template %s: %v", path, err)
	}

	return tpl
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
