package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

type Collection struct {
	ID    string
	Name  string
	Emoji string
	Count int
}

// timeout DB par défaut (aligné avec tastings.go)
const collectionsDBTimeout = 5 * time.Second

// ListCollections affiche la page principale listant toutes les collections
func ListCollections(w http.ResponseWriter, r *http.Request) {
	collections := GetCollections()

	data := struct {
		Collections []Collection
	}{
		Collections: collections,
	}

	if err := Tmpl.ExecuteTemplate(w, "collections_list.html", data); err != nil {
		log.Println("Erreur template collections_list:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
	}
}

func GetCollections() []Collection {
	ctx, cancel := context.WithTimeout(context.Background(), collectionsDBTimeout)
	defer cancel()

	rows, err := DB.QueryContext(ctx, `
		SELECT c.id, c.name, c.emoji, COUNT(ct.tasting_id)
		FROM collections c
		LEFT JOIN collection_tastings ct ON ct.collection_id = c.id
		GROUP BY c.id, c.name, c.emoji
		ORDER BY c.created_at DESC
	`)
	if err != nil {
		log.Println("Erreur collections:", err)
		return nil
	}
	defer rows.Close()

	var cols []Collection
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Name, &c.Emoji, &c.Count); err != nil {
			log.Println("Erreur scan collection:", err)
			continue
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		log.Println("Erreur rows collections:", err)
	}

	return cols
}

// ViewCollection affiche la page d'une collection avec ses dégustations
func ViewCollection(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), collectionsDBTimeout)
	defer cancel()

	var coll Collection
	err := DB.QueryRowContext(ctx, `SELECT id, name, emoji FROM collections WHERE id = $1`, id).
		Scan(&coll.ID, &coll.Name, &coll.Emoji)
	if err != nil {
		log.Println("Collection introuvable:", err)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	rows, err := DB.QueryContext(ctx, `
		SELECT
			t.id,
			t.product_name,
			COALESCE(t.maker,''),
			COALESCE(t.city,''),
			COALESCE(t.score,0),
			COALESCE(t.mode,'quick'),
			COALESCE(t.notes,''),
			COALESCE(t.photo_url,''),
			t.latitude,
			t.longitude,
			t.created_at,
			COALESCE(t.aroma_ids::text,'{}')
		FROM tastings t
		JOIN collection_tastings ct ON ct.tasting_id = t.id
		WHERE ct.collection_id = $1
		ORDER BY t.created_at DESC
	`, id)
	if err != nil {
		log.Println("Erreur requête collection tastings:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	allAromas := GetAromas()
	aMap := aromaMapFromSlice(allAromas)

	var tastings []Tasting
	var totalScore float64
	var scoredCount int // compter seulement les fiches avec une note
	cityCount := map[string]int{}

	for rows.Next() {
		var t Tasting
		var aromaIDsRaw string
		var lat, lng sql.NullFloat64

		if err := rows.Scan(
			&t.ID, &t.ProductName, &t.Maker, &t.City,
			&t.Score, &t.Mode, &t.Notes, &t.PhotoURL,
			&lat, &lng, &t.CreatedAt, &aromaIDsRaw,
		); err != nil {
			log.Println("Erreur scan:", err)
			continue
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
		for _, aid := range t.AromaIDs {
			if name, ok := aMap[aid]; ok {
				t.AromaNames = append(t.AromaNames, name)
			}
		}

		if t.Score > 0 {
			totalScore += t.Score
			scoredCount++
		}
		if t.City != "" {
			cityCount[t.City]++
		}

		tastings = append(tastings, t)
	}

	if err := rows.Err(); err != nil {
		log.Println("Erreur rows collection tastings:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
		return
	}

	// moyenne calculée uniquement sur les fiches notées
	avgScore := ""
	if scoredCount > 0 {
		avg := math.Round((totalScore/float64(scoredCount))*10) / 10
		s := fmt.Sprintf("%.1f", avg)
		if strings.HasSuffix(s, ".0") {
			s = s[:len(s)-2]
		}
		avgScore = s
	}

	topCity := ""
	topCount := 0
	for city, count := range cityCount {
		if count > topCount {
			topCity = city
			topCount = count
		}
	}

	data := struct {
		Collection Collection
		Tastings   []Tasting
		AvgScore   string
		TopCity    string
	}{
		Collection: coll,
		Tastings:   tastings,
		AvgScore:   avgScore,
		TopCity:    topCity,
	}

	if err := Tmpl.ExecuteTemplate(w, "collection.html", data); err != nil {
		log.Println("Erreur template collection:", err)
		http.Error(w, "Erreur serveur", http.StatusInternalServerError)
	}
}

func AddCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	emoji := strings.TrimSpace(r.FormValue("emoji"))
	if emoji == "" {
		emoji = "📁"
	}
	if name == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), collectionsDBTimeout)
	defer cancel()

	if _, err := DB.ExecContext(ctx, `INSERT INTO collections (name, emoji) VALUES ($1, $2)`, name, emoji); err != nil {
		log.Println("Erreur création collection:", err)
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func AddToCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "parse error"})
		return
	}

	collID := strings.TrimSpace(r.FormValue("collection_id"))
	tastingID := strings.TrimSpace(r.FormValue("tasting_id"))

	// Déterminer si la requête est AJAX
	isAjax := strings.Contains(r.Header.Get("Accept"), "application/json") ||
		strings.Contains(r.Header.Get("X-Requested-With"), "XMLHttpRequest")

	if collID == "" || tastingID == "" {
		if isAjax {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok":    false,
				"error": "collection_id ou tasting_id manquant",
			})
			return
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), collectionsDBTimeout)
	defer cancel()

	_, err := DB.ExecContext(ctx, `
		INSERT INTO collection_tastings (collection_id, tasting_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, collID, tastingID)
	if err != nil {
		log.Println("Erreur ajout collection:", err)
		if isAjax {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Récupérer le nom + emoji pour feedback
	var collName, collEmoji string
	_ = DB.QueryRowContext(ctx, `SELECT name, emoji FROM collections WHERE id = $1`, collID).
		Scan(&collName, &collEmoji)

	if isAjax {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              true,
			"collection_id":   collID,
			"collection_name": collName,
			"collection_emoji": func() string {
				if strings.TrimSpace(collEmoji) == "" {
					return "📁"
				}
				return collEmoji
			}(),
		})
		return
	}

	// Fallback formulaire HTML classique
	referer := r.Referer()
	if strings.Contains(referer, "/collections/view") {
		http.Redirect(w, r, referer, http.StatusFound)
	} else {
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func RemoveFromCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	_ = r.ParseForm()

	collID := strings.TrimSpace(r.FormValue("collection_id"))
	tastingID := strings.TrimSpace(r.FormValue("tasting_id"))

	if collID != "" && tastingID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), collectionsDBTimeout)
		defer cancel()
		_, _ = DB.ExecContext(ctx, `DELETE FROM collection_tastings WHERE collection_id=$1 AND tasting_id=$2`, collID, tastingID)
	}

	http.Redirect(w, r, "/collections/view?id="+collID, http.StatusFound)
}

func DeleteCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	_ = r.ParseForm()

	id := strings.TrimSpace(r.FormValue("id"))
	if id != "" {
		ctx, cancel := context.WithTimeout(r.Context(), collectionsDBTimeout)
		defer cancel()

		// supprimer d'abord les liaisons (si pas de CASCADE en DB)
		_, _ = DB.ExecContext(ctx, `DELETE FROM collection_tastings WHERE collection_id=$1`, id)
		_, _ = DB.ExecContext(ctx, `DELETE FROM collections WHERE id=$1`, id)
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

// writeJSON centralise l'encodage JSON (plus propre que des fmt.Fprintf avec échappement maison)

func CollectionsForTasting(w http.ResponseWriter, r *http.Request) {
	tid := strings.TrimSpace(r.URL.Query().Get("tasting_id"))
	if tid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "tasting_id manquant",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), collectionsDBTimeout)
	defer cancel()

	rows, err := DB.QueryContext(ctx, `
		SELECT c.id, c.name, COALESCE(c.emoji,'📁')
		FROM collections c
		JOIN collection_tastings ct ON ct.collection_id = c.id
		WHERE ct.tasting_id = $1
		ORDER BY c.created_at DESC
	`, tid)
	if err != nil {
		log.Println("Erreur CollectionsForTasting:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "erreur serveur",
		})
		return
	}
	defer rows.Close()

	type miniColl struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Emoji string `json:"emoji"`
	}

	var out []miniColl
	for rows.Next() {
		var c miniColl
		if err := rows.Scan(&c.ID, &c.Name, &c.Emoji); err != nil {
			log.Println("Scan miniColl:", err)
			continue
		}
		out = append(out, c)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"tasting_id":  tid,
		"collections": out,
	})
}
func RemoveFromCollectionAJAX(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "parse error"})
		return
	}

	collID := strings.TrimSpace(r.FormValue("collection_id"))
	tastingID := strings.TrimSpace(r.FormValue("tasting_id"))
	if collID == "" || tastingID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "collection_id ou tasting_id manquant"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), collectionsDBTimeout)
	defer cancel()

	if _, err := DB.ExecContext(ctx, `DELETE FROM collection_tastings WHERE collection_id=$1 AND tasting_id=$2`, collID, tastingID); err != nil {
		log.Println("RemoveFromCollectionAJAX:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "erreur serveur"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
func GetCollectionsForTasting(w http.ResponseWriter, r *http.Request) {
	tid := strings.TrimSpace(r.URL.Query().Get("tasting_id"))
	if tid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "tasting_id manquant"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), collectionsDBTimeout)
	defer cancel()

	rows, err := DB.QueryContext(ctx, `
		SELECT c.id, c.name, COALESCE(c.emoji,'📁')
		FROM collections c
		JOIN collection_tastings ct ON ct.collection_id = c.id
		WHERE ct.tasting_id = $1
		ORDER BY c.created_at DESC
	`, tid)
	if err != nil {
		log.Println("Erreur GetCollectionsForTasting:", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	defer rows.Close()

	type item struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Emoji string `json:"emoji"`
	}

	var out []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.Name, &it.Emoji); err != nil {
			continue
		}
		out = append(out, it)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "collections": out})
}
