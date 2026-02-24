package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nfnt/resize"
)

type Aroma struct {
	ID       int
	Name     string
	Family   string
	PhotoURL string
}

type Tasting struct {
	ID          string
	ProductName string
	Maker       string
	City        string
	Score       float64
	Mode        string
	Notes       string
	PhotoURL    string
	CreatedAt   time.Time

	AromaIDs   []int
	AromaNames []string

	Latitude  *float64
	Longitude *float64

	VueQuality   string
	SnapQuality  string
	MeltQuality  string
	FinishLength string
}

type HomeData struct {
	Tastings    []Tasting
	Aromas      []Aroma
	Collections []Collection
}

var DB *sql.DB
var Tmpl *template.Template

// Timeout DB par défaut (évite les requêtes coincées)
const dbTimeout = 5 * time.Second

// Upload & images
const (
	MaxUploadSize = 10 << 20 // 10MB
	MaxImageWidth = 1200     // large max (mobile-friendly)
	JpegQuality   = 80
)

// Client HTTP pour upload storage
var uploadHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
}

/* ─────────────────────────────────────────────
   Aromas helpers
───────────────────────────────────────────── */

func GetAromas() []Aroma {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	rows, err := DB.QueryContext(ctx, `SELECT id, name, family FROM aromas ORDER BY family, name`)
	if err != nil {
		log.Println("Erreur arômes:", err)
		return nil
	}
	defer rows.Close()

	var aromas []Aroma
	for rows.Next() {
		var a Aroma
		if err := rows.Scan(&a.ID, &a.Name, &a.Family); err != nil {
			log.Println("Erreur scan arômes:", err)
			continue
		}
		aromas = append(aromas, a)
	}
	if err := rows.Err(); err != nil {
		log.Println("Erreur rows arômes:", err)
	}
	return aromas
}

func aromaMapFromSlice(aromas []Aroma) map[int]string {
	m := make(map[int]string, len(aromas))
	for _, a := range aromas {
		m[a.ID] = a.Name
	}
	return m
}

// Parse "{1,3,5}" -> []int (Postgres int[])
func parsePgIntArray(raw string) []int {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "{}")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

/* ─────────────────────────────────────────────
   Scan tasting
───────────────────────────────────────────── */

const tastingSelectCols = `
	id,
	product_name,
	COALESCE(maker,''),
	COALESCE(city,''),
	COALESCE(score,0),
	COALESCE(mode,'quick'),
	COALESCE(notes,''),
	COALESCE(photo_url,''),
	latitude,
	longitude,
	created_at,
	COALESCE(aroma_ids::text,'{}'),
	COALESCE(vue_quality,''),
	COALESCE(snap_quality,''),
	COALESCE(melt_quality,''),
	COALESCE(finish_length,'')
`

// scanTasting scanne une ligne DB en Tasting.
// Ordre attendu = tastingSelectCols
func scanTasting(row interface {
	Scan(...any) error
}, aromaMap map[int]string) (Tasting, error) {
	var t Tasting
	var aromaIDsRaw string
	var lat, lng sql.NullFloat64

	err := row.Scan(
		&t.ID, &t.ProductName, &t.Maker, &t.City,
		&t.Score, &t.Mode, &t.Notes, &t.PhotoURL,
		&lat, &lng, &t.CreatedAt, &aromaIDsRaw,
		&t.VueQuality, &t.SnapQuality, &t.MeltQuality, &t.FinishLength,
	)
	if err != nil {
		return t, err
	}

	if lat.Valid {
		v := lat.Float64
		t.Latitude = &v
	}
	if lng.Valid {
		v := lng.Float64
		t.Longitude = &v
	}

	t.AromaIDs = parsePgIntArray(aromaIDsRaw)
	for _, id := range t.AromaIDs {
		if name, ok := aromaMap[id]; ok {
			t.AromaNames = append(t.AromaNames, name)
		}
	}
	return t, nil
}

/* ─────────────────────────────────────────────
   Pages
───────────────────────────────────────────── */

func Home(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	rows, err := DB.QueryContext(ctx, `SELECT`+tastingSelectCols+`FROM tastings ORDER BY created_at DESC`)
	if err != nil {
		log.Println("Erreur requête:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	allAromas := GetAromas()
	aMap := aromaMapFromSlice(allAromas)

	var tastings []Tasting
	for rows.Next() {
		t, err := scanTasting(rows, aMap)
		if err != nil {
			log.Println("Erreur scan:", err)
			continue
		}
		tastings = append(tastings, t)
	}
	if err := rows.Err(); err != nil {
		log.Println("Erreur rows tastings:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
		return
	}

	data := HomeData{
		Tastings:    tastings,
		Aromas:      allAromas,
		Collections: GetCollections(),
	}

	if err := Tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		log.Println("Erreur template:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
	}
}

/* ─────────────────────────────────────────────
   Add / Update helpers
───────────────────────────────────────────── */

// buildNotes assemble les champs du formulaire (rapide ou approfondi) en une note complète.
func buildNotes(r *http.Request) string {
	mode := strings.TrimSpace(r.FormValue("mode"))
	if mode != "deep" {
		return strings.TrimSpace(r.FormValue("notes"))
	}

	var parts []string

	if v := strings.TrimSpace(r.FormValue("vue_quality")); v != "" {
		parts = append(parts, "Vue : "+v)
	}
	if v := strings.TrimSpace(r.FormValue("snap_quality")); v != "" {
		parts = append(parts, "Cassant : "+v)
	}
	if v := strings.TrimSpace(r.FormValue("notes_cassant")); v != "" {
		parts = append(parts, v)
	}
	if v := strings.TrimSpace(r.FormValue("melt_quality")); v != "" {
		parts = append(parts, "Texture : "+v)
	}
	if v := strings.TrimSpace(r.FormValue("finish_length")); v != "" {
		parts = append(parts, "Finale : "+v)
	}
	if v := strings.TrimSpace(r.FormValue("notes_finale")); v != "" {
		parts = append(parts, v)
	}
	if v := strings.TrimSpace(r.FormValue("notes")); v != "" {
		parts = append(parts, v)
	}

	return strings.Join(parts, "\n")
}

// parse float safe
func parseFloatOrNull(s string) sql.NullFloat64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullFloat64{Valid: false}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return sql.NullFloat64{Valid: false}
	}
	return sql.NullFloat64{Float64: f, Valid: true}
}

func buildPgIntArray(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	// garde-fou : ne garder que des chiffres
	clean := make([]string, 0, len(ids))
	for _, s := range ids {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, err := strconv.Atoi(s); err == nil {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return "{}"
	}
	return "{" + strings.Join(clean, ",") + "}"
}

/* ─────────────────────────────────────────────
   ADD TASTING (avec limites + transaction DB)
───────────────────────────────────────────── */

func AddTasting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Limite dure : si quelqu’un tente 200MB, on coupe net.
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadSize)

	if err := r.ParseMultipartForm(MaxUploadSize); err != nil {
		log.Println("Erreur ParseMultipartForm:", err)
		http.Error(w, "Fichier trop lourd (max 10MB)", http.StatusBadRequest)
		return
	}

	productName := strings.TrimSpace(r.FormValue("product_name"))
	if productName == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	maker := strings.TrimSpace(r.FormValue("maker"))
	city := strings.TrimSpace(r.FormValue("city"))

	mode := strings.TrimSpace(r.FormValue("mode"))
	if mode == "" {
		mode = "quick"
	}

	notes := buildNotes(r)

	vueQ := strings.TrimSpace(r.FormValue("vue_quality"))
	snapQ := strings.TrimSpace(r.FormValue("snap_quality"))
	meltQ := strings.TrimSpace(r.FormValue("melt_quality"))
	finishL := strings.TrimSpace(r.FormValue("finish_length"))

	// En mode quick, on vide pour ne pas polluer
	if mode != "deep" {
		vueQ, snapQ, meltQ, finishL = "", "", "", ""
	}

	scoreVal := 0.0
	if s := strings.TrimSpace(r.FormValue("score")); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			scoreVal = f
		}
	}

	lat := parseFloatOrNull(r.FormValue("latitude"))
	lng := parseFloatOrNull(r.FormValue("longitude"))

	aromaArray := buildPgIntArray(r.Form["aroma_ids"])

	// 1) Transaction DB : on crée la dégustation, on récupère l’ID
	var tastingID string
	{
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		tx, err := DB.BeginTx(ctx, nil)
		if err != nil {
			log.Println("Erreur BeginTx:", err)
			http.Error(w, "Erreur serveur", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		err = tx.QueryRowContext(ctx, `
			INSERT INTO tastings (
				product_name, maker, city, score, notes, mode,
				aroma_ids, latitude, longitude,
				vue_quality, snap_quality, melt_quality, finish_length,
				photo_url
			)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			RETURNING id
		`,
			productName, maker, city, scoreVal, notes, mode,
			aromaArray, lat, lng,
			vueQ, snapQ, meltQ, finishL,
			"", // photo_url sera mis à jour après upload si dispo
		).Scan(&tastingID)

		if err != nil {
			log.Println("Erreur insertion:", err)
			http.Error(w, "Erreur sauvegarde", http.StatusInternalServerError)
			return
		}

		if err := tx.Commit(); err != nil {
			log.Println("Erreur commit:", err)
			http.Error(w, "Erreur sauvegarde", http.StatusInternalServerError)
			return
		}
	}

	// 2) Upload photo (hors transaction DB)
	file, header, err := r.FormFile("photo")
	if err == nil {
		defer file.Close()

		photoURL, upErr := processAndUploadImage(r.Context(), file, header, tastingID)
		if upErr != nil {
			log.Println("Erreur upload photo:", upErr)
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
			defer cancel()

			if _, upDBErr := DB.ExecContext(ctx, `UPDATE tastings SET photo_url=$1 WHERE id=$2`, photoURL, tastingID); upDBErr != nil {
				log.Println("Erreur update photo_url:", upDBErr)
			}
		}
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

/* ─────────────────────────────────────────────
   DELETE / EDIT / UPDATE
───────────────────────────────────────────── */

func DeleteTasting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Supprimer d'abord les liaisons collections (si pas de CASCADE)
	{
		ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
		defer cancel()
		if _, err := DB.ExecContext(ctx, `DELETE FROM collection_tastings WHERE tasting_id = $1`, id); err != nil {
			log.Println("Erreur suppression collection_tastings:", err)
		}
	}

	{
		ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
		defer cancel()
		if _, err := DB.ExecContext(ctx, `DELETE FROM tastings WHERE id = $1`, id); err != nil {
			log.Println("Erreur suppression:", err)
		}
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func EditForm(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	allAromas := GetAromas()
	aMap := aromaMapFromSlice(allAromas)

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	row := DB.QueryRowContext(ctx, `SELECT`+tastingSelectCols+`FROM tastings WHERE id = $1`, id)
	t, err := scanTasting(row, aMap)
	if err != nil {
		log.Println("Erreur lecture:", err)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	data := struct {
		Tasting Tasting
		Aromas  []Aroma
	}{t, allAromas}

	if err := Tmpl.ExecuteTemplate(w, "edit.html", data); err != nil {
		log.Println("Erreur template edit:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
	}
}

func UpdateTasting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadSize)
	if err := r.ParseMultipartForm(MaxUploadSize); err != nil {
		log.Println("Erreur ParseMultipartForm:", err)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	productName := strings.TrimSpace(r.FormValue("product_name"))
	maker := strings.TrimSpace(r.FormValue("maker"))
	city := strings.TrimSpace(r.FormValue("city"))

	mode := strings.TrimSpace(r.FormValue("mode"))
	if mode == "" {
		mode = "quick"
	}

	notes := buildNotes(r)

	vueQ := strings.TrimSpace(r.FormValue("vue_quality"))
	snapQ := strings.TrimSpace(r.FormValue("snap_quality"))
	meltQ := strings.TrimSpace(r.FormValue("melt_quality"))
	finishL := strings.TrimSpace(r.FormValue("finish_length"))

	if mode != "deep" {
		vueQ, snapQ, meltQ, finishL = "", "", "", ""
	}

	scoreVal := 0.0
	if s := strings.TrimSpace(r.FormValue("score")); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			scoreVal = f
		}
	}

	lat := parseFloatOrNull(r.FormValue("latitude"))
	lng := parseFloatOrNull(r.FormValue("longitude"))

	aromaArray := buildPgIntArray(r.Form["aroma_ids"])

	{
		ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
		defer cancel()

		_, err := DB.ExecContext(ctx, `
			UPDATE tastings
			SET product_name=$1, maker=$2, city=$3, score=$4, notes=$5, mode=$6,
				aroma_ids=$7, latitude=$8, longitude=$9,
				vue_quality=$10, snap_quality=$11, melt_quality=$12, finish_length=$13
			WHERE id=$14
		`,
			productName, maker, city, scoreVal, notes, mode,
			aromaArray, lat, lng,
			vueQ, snapQ, meltQ, finishL,
			id,
		)

		if err != nil {
			log.Println("Erreur mise à jour:", err)
			http.Error(w, "Erreur sauvegarde", http.StatusInternalServerError)
			return
		}
	}

	// Photo (optionnelle)
	file, header, err := r.FormFile("photo")
	if err == nil {
		defer file.Close()

		photoURL, upErr := processAndUploadImage(r.Context(), file, header, id)
		if upErr != nil {
			log.Println("Erreur upload photo:", upErr)
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
			defer cancel()

			if _, upDBErr := DB.ExecContext(ctx, `UPDATE tastings SET photo_url=$1 WHERE id=$2`, photoURL, id); upDBErr != nil {
				log.Println("Erreur update photo_url:", upDBErr)
			}
		}
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

/* ─────────────────────────────────────────────
   MAP
───────────────────────────────────────────── */

func MapView(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	rows, err := DB.QueryContext(ctx, `SELECT`+tastingSelectCols+`FROM tastings ORDER BY created_at DESC`)
	if err != nil {
		log.Println("Erreur requête map:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	aMap := aromaMapFromSlice(GetAromas())

	var tastings []Tasting
	cities := map[string]bool{}

	for rows.Next() {
		t, err := scanTasting(rows, aMap)
		if err != nil {
			log.Println("Erreur scan map:", err)
			continue
		}
		if t.City != "" {
			cities[t.City] = true
		}
		tastings = append(tastings, t)
	}
	if err := rows.Err(); err != nil {
		log.Println("Erreur rows map:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
		return
	}

	data := struct {
		Tastings  []Tasting
		CityCount int
	}{
		Tastings:  tastings,
		CityCount: len(cities),
	}

	var buf bytes.Buffer
	if err := Tmpl.ExecuteTemplate(&buf, "map.html", data); err != nil {
		log.Println("Erreur template map:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

/* ─────────────────────────────────────────────
   IMAGE PROCESS + UPLOAD (resize + jpeg)
───────────────────────────────────────────── */

func processAndUploadImage(ctx context.Context, file multipart.File, header *multipart.FileHeader, tastingID string) (string, error) {
	supabaseURL := strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	jwtKey := strings.TrimSpace(os.Getenv("SUPABASE_SERVICE_ROLE_KEY"))
	if supabaseURL == "" || jwtKey == "" {
		return "", fmt.Errorf("SUPABASE_URL ou SUPABASE_SERVICE_ROLE_KEY manquant")
	}

	// Petit garde-fou
	if header != nil && header.Size > MaxUploadSize {
		return "", fmt.Errorf("fichier trop volumineux (max 10MB)")
	}

	// Décodage image (jpeg/png/webp si dispo via stdlib: jpeg/png ok; webp non par défaut)
	img, format, err := image.Decode(file)
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	_ = format

	// Resize si trop large (on garde le ratio)
	b := img.Bounds()
	if b.Dx() > MaxImageWidth {
		img = resize.Resize(MaxImageWidth, 0, img, resize.Lanczos3)
	}

	// Encodage JPEG qualité 80
	buf := new(bytes.Buffer)
	if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: JpegQuality}); err != nil {
		return "", fmt.Errorf("encode jpeg: %w", err)
	}

	// Nom de fichier : toujours .jpg après compression
	fileName := fmt.Sprintf("tasting-%s-%d.jpg", tastingID, time.Now().Unix())

	uploadURL := supabaseURL + "/storage/v1/object/photos/" + fileName

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+jwtKey)
	req.Header.Set("apikey", jwtKey)
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("x-upsert", "true")

	resp, err := uploadHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &httpError{Status: resp.Status, Body: string(body)}
	}

	publicURL := supabaseURL + "/storage/v1/object/public/photos/" + fileName
	return publicURL, nil
}

/* ─────────────────────────────────────────────
   Errors
───────────────────────────────────────────── */

type httpError struct {
	Status string
	Body   string
}

func (e *httpError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + " - " + e.Body
}

/* ─────────────────────────────────────────────
   NOTE: imports "mime" / "filepath" conservés ?
   -> Ici on n'en a plus besoin pour l'upload (tout sort en jpeg),
      mais si tu veux garder la logique "extension originale" tu peux.
───────────────────────────────────────────── */

// Pour éviter les imports inutilisés si tu colles tel quel :
var _ = mime.TypeByExtension
var _ = filepath.Ext
