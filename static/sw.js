// Cacao — Service Worker
// Objectif : rendre la PWA plus "app-like" avec un fallback hors-ligne propre.
// Stratégie :
// - Precache minimal (home + offline + manifest + icônes)
// - Navigation (pages HTML) : network-first, fallback offline
// - Assets (images/css/js) : cache-first léger
// - API / requêtes non-GET : on laisse passer (pas de cache)

const CACHE_NAME = "cacao-v1";
const OFFLINE_URL = "/offline";

// Ressources essentielles à mettre en cache au premier chargement.
// (Important : éviter les URL externes type Google Fonts ici, souvent bloquées par CORS en cache.addAll)
const PRECACHE_URLS = [
  "/",
  OFFLINE_URL,
  "/manifest.json",
  "/icon-192.png",
  "/icon-512.png",
  "/sw.js",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then(async (cache) => {
      for (const url of PRECACHE_URLS) {
        try {
          await cache.add(url);
        } catch (e) {
          // On ne bloque pas l'installation si une ressource échoue (dev/local/icone manquante…)
        }
      }
    })
  );
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE_NAME).map((k) => caches.delete(k)))
    )
  );
  self.clients.claim();
});

self.addEventListener("fetch", (event) => {
  // On ne gère que les GET (pas de cache pour POST / upload photo / etc.)
  if (event.request.method !== "GET") return;

  const url = new URL(event.request.url);

  // Ne pas interférer avec les extensions, etc.
  if (url.protocol !== "http:" && url.protocol !== "https:") return;

  // Si tu as des appels externes (ex: supabase, analytics), on ne les cache pas
  // (tu peux ajouter d'autres domaines si besoin)
  if (url.hostname.includes("supabase.co")) return;

  // 1) NAVIGATION (pages HTML)
  // On veut : réseau d'abord (données fraîches), sinon page offline.
  if (event.request.mode === "navigate") {
    event.respondWith(networkFirstForPages(event.request));
    return;
  }

  // 2) ASSETS (css/js/images/fonts...) : cache-first léger
  // Ça améliore énormément les temps de chargement.
  event.respondWith(cacheFirstForAssets(event.request));
});

// --- Stratégies ---

async function networkFirstForPages(request) {
  try {
    const response = await fetch(request);

    // On met en cache la page si OK (utile pour accès ultérieur)
    if (response && response.ok) {
      const cache = await caches.open(CACHE_NAME);
      cache.put(request, response.clone());
    }
    return response;
  } catch (err) {
    // Hors-ligne → tente cache, sinon page offline
    const cached = await caches.match(request);
    if (cached) return cached;

    const offline = await caches.match(OFFLINE_URL);
    if (offline) return offline;

    // Ultime fallback
    return new Response("Hors ligne", {
      status: 200,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  }
}

async function cacheFirstForAssets(request) {
  const cached = await caches.match(request);
  if (cached) return cached;

  try {
    const response = await fetch(request);

    // Cache uniquement les réponses valides
    if (response && response.ok) {
      const cache = await caches.open(CACHE_NAME);
      cache.put(request, response.clone());
    }
    return response;
  } catch (err) {
    // Si un asset échoue hors-ligne, on renvoie juste l'erreur (pas de fallback HTML ici)
    return cached || Response.error();
  }
}