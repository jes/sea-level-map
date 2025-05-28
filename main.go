package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// Cache structure for storing generated tiles
type TileCache struct {
	mu    sync.RWMutex
	tiles map[string]CachedTile
}

type CachedTile struct {
	data      []byte
	timestamp time.Time
}

var cache = &TileCache{
	tiles: make(map[string]CachedTile),
}

const (
	tileSize = 256
)

// generateSeaLevelTile fetches elevation data and creates a blue tile for areas above sea level
func generateSeaLevelTile(z, x, y string) ([]byte, error) {
	// Create cache key
	cacheKey := fmt.Sprintf("%s/%s/%s", z, x, y)

	// Check cache first
	cache.mu.RLock()
	if cached, exists := cache.tiles[cacheKey]; exists {
		cache.mu.RUnlock()
		log.Printf("Cache hit for tile: z=%s, x=%s, y=%s", z, x, y)
		return cached.data, nil
	}
	cache.mu.RUnlock()

	// Fetch elevation data from terrarium tiles
	elevationURL := fmt.Sprintf("https://s3.amazonaws.com/elevation-tiles-prod/terrarium/%s/%s/%s.png", z, x, y)

	resp, err := http.Get(elevationURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch elevation tile: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("elevation tile request failed with status: %d", resp.StatusCode)
	}

	// Decode the elevation PNG
	elevationImg, err := png.Decode(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to decode elevation PNG: %v", err)
	}

	// Convert to RGBA if it's not already
	var rgbaImg *image.RGBA
	if rgba, ok := elevationImg.(*image.RGBA); ok {
		rgbaImg = rgba
	} else {
		bounds := elevationImg.Bounds()
		rgbaImg = image.NewRGBA(bounds)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				rgbaImg.Set(x, y, elevationImg.At(x, y))
			}
		}
	}

	// Create output image
	outputImg := image.NewRGBA(image.Rect(0, 0, tileSize, tileSize))

	// Process image in parallel using goroutines
	numWorkers := 8 // Adjust based on your CPU cores
	rowsPerWorker := tileSize / numWorkers
	var wg sync.WaitGroup

	for worker := 0; worker < numWorkers; worker++ {
		wg.Add(1)
		go func(startRow, endRow int) {
			defer wg.Done()

			// Blue color for areas below sea level (underwater)
			blue := [4]uint8{0, 100, 200, 255}
			transparent := [4]uint8{0, 0, 0, 0}

			for y := startRow; y < endRow && y < tileSize; y++ {
				for x := 0; x < tileSize; x++ {
					// Calculate pixel offset in the byte array
					srcOffset := (y*rgbaImg.Stride + x*4)
					dstOffset := (y*outputImg.Stride + x*4)

					// Get RGB values directly from byte array
					if srcOffset+2 < len(rgbaImg.Pix) {
						rVal := rgbaImg.Pix[srcOffset]
						gVal := rgbaImg.Pix[srcOffset+1]
						bVal := rgbaImg.Pix[srcOffset+2]

						// Decode terrarium format: elevation = (R * 256 + G + B / 256) - 32768
						// Using integer arithmetic for better performance
						elevation := int(rVal)*256 + int(gVal) + int(bVal)/256 - 32768

						// If elevation is below sea level (negative), make it blue, otherwise transparent
						var color [4]uint8
						if elevation < 0 {
							color = blue
						} else {
							color = transparent
						}

						// Set pixel directly in byte array
						if dstOffset+3 < len(outputImg.Pix) {
							outputImg.Pix[dstOffset] = color[0]   // R
							outputImg.Pix[dstOffset+1] = color[1] // G
							outputImg.Pix[dstOffset+2] = color[2] // B
							outputImg.Pix[dstOffset+3] = color[3] // A
						}
					}
				}
			}
		}(worker*rowsPerWorker, (worker+1)*rowsPerWorker)
	}

	// Wait for all workers to complete
	wg.Wait()

	// Encode to PNG bytes
	var buf bytes.Buffer
	err = png.Encode(&buf, outputImg)
	if err != nil {
		return nil, fmt.Errorf("failed to encode output PNG: %v", err)
	}

	tileData := buf.Bytes()

	// Cache the result
	cache.mu.Lock()
	cache.tiles[cacheKey] = CachedTile{
		data:      tileData,
		timestamp: time.Now(),
	}
	cache.mu.Unlock()

	log.Printf("Generated and cached tile: z=%s, x=%s, y=%s", z, x, y)
	return tileData, nil
}

// serveIndex serves the index.html file
func serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

// serveTile serves a sea level tile
func serveTile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	z := vars["z"]
	x := vars["x"]
	y := vars["y"]

	// Validate that z, x, y are valid integers
	if _, err := strconv.Atoi(z); err != nil {
		http.Error(w, "Invalid zoom level", http.StatusBadRequest)
		return
	}
	if _, err := strconv.Atoi(x); err != nil {
		http.Error(w, "Invalid x coordinate", http.StatusBadRequest)
		return
	}
	if _, err := strconv.Atoi(y); err != nil {
		http.Error(w, "Invalid y coordinate", http.StatusBadRequest)
		return
	}

	// Generate sea level tile
	tileData, err := generateSeaLevelTile(z, x, y)
	if err != nil {
		http.Error(w, "Failed to generate tile", http.StatusInternalServerError)
		log.Printf("Error generating tile: %v", err)
		return
	}

	// Set appropriate headers
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600") // Cache for 1 hour
	w.Header().Set("Access-Control-Allow-Origin", "*")      // Allow CORS

	// Write the tile data
	w.Write(tileData)

	log.Printf("Served tile: z=%s, x=%s, y=%s", z, x, y)
}

func main() {
	// Check if index.html exists
	if _, err := os.Stat("index.html"); os.IsNotExist(err) {
		log.Fatal("index.html file not found in current directory")
	}

	// Create router
	r := mux.NewRouter()

	// Routes
	r.HandleFunc("/", serveIndex).Methods("GET")
	r.HandleFunc("/tile/{z:[0-9]+}/{x:[0-9]+}/{y:[0-9]+}.png", serveTile).Methods("GET")

	// Add some logging middleware
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("%s %s", r.Method, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	})

	port := "8080"
	if envPort := os.Getenv("PORT"); envPort != "" {
		port = envPort
	}

	log.Printf("Starting sea level map server on port %s", port)
	log.Printf("Visit http://localhost:%s to view the map", port)
	log.Printf("Tile endpoint: http://localhost:%s/tile/{z}/{x}/{y}.png", port)

	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
