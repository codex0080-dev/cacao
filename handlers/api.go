package handlers

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────
// Helpers JSON
// ─────────────────────────────────────────────────────────────

func writeEmptyArray(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte("[]"))
}

// ─── Autocomplete produits ─────────────────────────────────────────────────

// ProductSuggest renvoie les noms de produits correspondant à la requête `q`.
// Amélioré : cherche aussi dans maker.
type ProductSuggestion struct {
	Name  string `json:"name"`
	Maker string `json:"maker"`
}

func ProductSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeJSON(w, http.StatusOK, []ProductSuggestion{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	needle := "%" + q + "%"

	rows, err := DB.QueryContext(ctx, `
		SELECT DISTINCT product_name, COALESCE(maker,'')
		FROM tastings
		WHERE product_name ILIKE $1 OR maker ILIKE $1
		ORDER BY product_name
		LIMIT 10
	`, needle)
	if err != nil {
		log.Println("Erreur autocomplete:", err)
		writeJSON(w, http.StatusOK, []ProductSuggestion{})
		return
	}
	defer rows.Close()

	out := make([]ProductSuggestion, 0, 10)
	for rows.Next() {
		var s ProductSuggestion
		if err := rows.Scan(&s.Name, &s.Maker); err != nil {
			continue
		}
		s.Name = strings.TrimSpace(s.Name)
		s.Maker = strings.TrimSpace(s.Maker)
		if s.Name != "" {
			out = append(out, s)
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// ─── Geo proxy (cache simple en mémoire) ───────────────────────────────────

type geoCache struct {
	mu      sync.RWMutex
	entries map[string]geoCacheEntry
}

type geoCacheEntry struct {
	body      []byte
	expiresAt time.Time
}

var geoCache_ = &geoCache{entries: make(map[string]geoCacheEntry)}

// Nettoyage opportuniste : toutes les X écritures, on vire les entrées expirées
var geoCacheSetCount int

func (c *geoCache) get(key string) ([]byte, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.body, true
}

func (c *geoCache) set(key string, body []byte, ttl time.Duration) {
	c.mu.Lock()
	c.entries[key] = geoCacheEntry{body: body, expiresAt: time.Now().Add(ttl)}
	geoCacheSetCount++
	doCleanup := geoCacheSetCount%50 == 0
	c.mu.Unlock()

	if doCleanup {
		c.cleanupExpired()
	}
}

func (c *geoCache) cleanupExpired() {
	now := time.Now()
	c.mu.Lock()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

var geoHTTPClient = &http.Client{
	Timeout: 6 * time.Second,
}

func nominatimUserAgent() string {
	// IMPORTANT : mets un vrai contact en prod (email/site)
	if ua := strings.TrimSpace(os.Getenv("NOMINATIM_USER_AGENT")); ua != "" {
		return ua
	}
	return "Cacao-App/1.0 (+https://example.com; contact@example.com)"
}

func nominatimEmailParam() string {
	// Recommandé par Nominatim (pas obligatoire mais apprécié)
	return strings.TrimSpace(os.Getenv("NOMINATIM_EMAIL"))
}

func nominatimProxy(nominatimURL string, w http.ResponseWriter, r *http.Request) {
	if body, ok := geoCache_.get(nominatimURL); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, nominatimURL, nil)
	if err != nil {
		http.Error(w, "Erreur requête geo", http.StatusInternalServerError)
		return
	}

	req.Header.Set("User-Agent", nominatimUserAgent())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "fr")

	resp, err := geoHTTPClient.Do(req)
	if err != nil {
		http.Error(w, "Service géolocalisation indisponible", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Erreur lecture réponse geo", http.StatusInternalServerError)
		return
	}

	// Cache seulement si OK et non vide
	if resp.StatusCode == http.StatusOK && len(body) > 0 {
		geoCache_.set(nominatimURL, body, 24*time.Hour)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// GeoSearch proxifie la recherche Nominatim.
// GET /api/geo/search?q=Paris
func GeoSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeEmptyArray(w)
		return
	}

	base := "https://nominatim.openstreetmap.org/search"
	v := url.Values{}
	v.Set("format", "json")
	v.Set("q", q)
	v.Set("limit", "6")
	v.Set("addressdetails", "1")
	v.Set("accept-language", "fr")
	if em := nominatimEmailParam(); em != "" {
		v.Set("email", em)
	}

	nominatimProxy(base+"?"+v.Encode(), w, r)
}

// GeoReverse proxifie le géocodage inverse Nominatim.
// GET /api/geo/reverse?lat=48.85&lon=2.35
func GeoReverse(w http.ResponseWriter, r *http.Request) {
	lat := strings.TrimSpace(r.URL.Query().Get("lat"))
	lon := strings.TrimSpace(r.URL.Query().Get("lon"))
	if lat == "" || lon == "" {
		http.Error(w, "lat et lon requis", http.StatusBadRequest)
		return
	}

	// garde-fou simple
	if len(lat) > 20 || len(lon) > 20 {
		http.Error(w, "lat/lon invalides", http.StatusBadRequest)
		return
	}

	base := "https://nominatim.openstreetmap.org/reverse"
	v := url.Values{}
	v.Set("format", "json")
	v.Set("lat", lat)
	v.Set("lon", lon)
	v.Set("addressdetails", "1")
	v.Set("accept-language", "fr")
	if em := nominatimEmailParam(); em != "" {
		v.Set("email", em)
	}

	nominatimProxy(base+"?"+v.Encode(), w, r)
}

// (Optionnel) helper si tu veux l'utiliser ailleurs
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
