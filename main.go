package main

import (
	"cacao/handlers"
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

// Middleware log simple (utile en dev + prod)
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.Method, r.RequestURI, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

func main() {
	// Charge .env si présent (en prod, ça peut ne pas exister, et c'est OK)
	_ = godotenv.Load()

	// --- DB ---
	dsn := os.Getenv("SUPABASE_DB_URL")
	if dsn == "" {
		log.Fatal("❌ SUPABASE_DB_URL est vide. Mets-la dans .env ou dans tes variables d'environnement.")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal("❌ Erreur connexion DB:", err)
	}
	defer db.Close()

	// Optionnel mais utile : un pool raisonnable
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	// Ping avec timeout pour éviter un freeze infini
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := db.PingContext(ctx); err != nil {
			log.Fatal("❌ Impossible de joindre Supabase:", err)
		}
	}

	fmt.Println("✅ Connecté à Supabase !")

	// --- Templates ---
	funcMap := template.FuncMap{
		"f64": func(p *float64) float64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"fmtScore": func(f float64) string {
			s := strconv.FormatFloat(f, 'f', 1, 64)
			if len(s) > 2 && s[len(s)-2:] == ".0" {
				return s[:len(s)-2]
			}
			return s
		},
	}

	tmpl := template.Must(
		template.New("").Funcs(funcMap).ParseGlob("templates/*.html"),
	)

	handlers.DB = db
	handlers.Tmpl = tmpl

	// --- Router ---
	mux := http.NewServeMux()

	// Fichiers statiques PWA
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFile(w, r, "static/manifest.json")
	})

	mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Service-Worker-Allowed", "/")
		http.ServeFile(w, r, "static/sw.js")
	})

	mux.HandleFunc("/icon-192.png", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/icon-192.png")
	})
	mux.HandleFunc("/icon-512.png", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/icon-512.png")
	})

	// Routes app
	mux.HandleFunc("/", handlers.Home)
	mux.HandleFunc("/add", handlers.AddTasting)
	mux.HandleFunc("/delete", handlers.DeleteTasting)
	mux.HandleFunc("/edit", handlers.EditForm)
	mux.HandleFunc("/update", handlers.UpdateTasting)

	mux.HandleFunc("/offline", func(w http.ResponseWriter, r *http.Request) {
		tmpl.ExecuteTemplate(w, "offline.html", nil)
	})

	// Collections
	mux.HandleFunc("/collections", handlers.ListCollections)
	mux.HandleFunc("/collections/view", handlers.ViewCollection)
	mux.HandleFunc("/collections/add", handlers.AddCollection)
	mux.HandleFunc("/collections/addtasting", handlers.AddToCollection)
	mux.HandleFunc("/collections/remove", handlers.RemoveFromCollection)
	mux.HandleFunc("/collections/delete", handlers.DeleteCollection)
	mux.HandleFunc("/collections/for", handlers.CollectionsForTasting)
	mux.HandleFunc("/collections/remove-ajax", handlers.RemoveFromCollectionAJAX)

	// Carte
	mux.HandleFunc("/map", handlers.MapView)

	// API — autocomplete + geo proxy
	mux.HandleFunc("/api/products", handlers.ProductSuggest)
	mux.HandleFunc("/api/geo/search", handlers.GeoSearch)
	mux.HandleFunc("/api/geo/reverse", handlers.GeoReverse)

	// Petit endpoint de vie (pratique pour tester vite fait)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// --- Server ---
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	log.Printf("🚀 Serveur sur http://localhost%s", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(mux), // ✅ on applique le middleware ici
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Fatal(srv.ListenAndServe())
}
