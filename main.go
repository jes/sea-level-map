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
	mu       sync.RWMutex
	tiles    map[string]CachedTile
	inFlight map[string]chan []byte // Track in-flight requests
	flightMu sync.Mutex
}

type CachedTile struct {
	data      []byte
	timestamp time.Time
}

var cache = &TileCache{
	tiles:    make(map[string]CachedTile),
	inFlight: make(map[string]chan []byte),
}

const (
	tileSize = 256
)

// clampSeaLevel ensures the sea level is within valid bounds and rounded to 10m increments
func clampSeaLevel(level int) int {
	// Round to nearest 10m increment
	level = ((level + 5) / 10) * 10

	// Clamp to valid range
	if level < -1000 {
		level = -1000
	} else if level > 1000 {
		level = 1000
	}

	return level
}

// generateSeaLevelTile fetches elevation data and creates a blue tile for areas above sea level
func generateSeaLevelTile(seaLevel int, z, x, y string) ([]byte, error) {
	// Create cache key that includes sea level
	cacheKey := fmt.Sprintf("%d/%s/%s/%s", seaLevel, z, x, y)

	// Check cache first
	cache.mu.RLock()
	if cached, exists := cache.tiles[cacheKey]; exists {
		cache.mu.RUnlock()
		log.Printf("Cache hit for tile: level=%d, z=%s, x=%s, y=%s", seaLevel, z, x, y)
		return cached.data, nil
	}
	cache.mu.RUnlock()

	// Check if another goroutine is already processing this tile
	cache.flightMu.Lock()
	if ch, exists := cache.inFlight[cacheKey]; exists {
		// Another request is in flight, wait for it
		cache.flightMu.Unlock()
		log.Printf("Waiting for in-flight tile: level=%d, z=%s, x=%s, y=%s", seaLevel, z, x, y)
		data := <-ch
		return data, nil
	}

	// Mark this request as in-flight
	ch := make(chan []byte, 1)
	cache.inFlight[cacheKey] = ch
	cache.flightMu.Unlock()

	// Ensure we clean up the in-flight marker
	defer func() {
		cache.flightMu.Lock()
		delete(cache.inFlight, cacheKey)
		cache.flightMu.Unlock()
	}()

	// Fetch elevation data from terrarium tiles
	elevationURL := fmt.Sprintf("https://s3.amazonaws.com/elevation-tiles-prod/terrarium/%s/%s/%s.png", z, x, y)

	log.Printf("Fetching upstream tile: level=%d, z=%s, x=%s, y=%s", seaLevel, z, x, y)
	fetchStart := time.Now()

	// Create HTTP request with user-agent
	req, err := http.NewRequest("GET", elevationURL, nil)
	if err != nil {
		close(ch) // Signal waiting goroutines that we failed
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Set user-agent header
	req.Header.Set("User-Agent", "SeaLevelMap/1.0 (https://github.com/jes/sea-level-map)")

	// Execute the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		close(ch) // Signal waiting goroutines that we failed
		return nil, fmt.Errorf("failed to fetch elevation tile: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		close(ch) // Signal waiting goroutines that we failed
		return nil, fmt.Errorf("elevation tile request failed with status: %d", resp.StatusCode)
	}

	// Decode the elevation PNG
	elevationImg, err := png.Decode(resp.Body)
	if err != nil {
		close(ch) // Signal waiting goroutines that we failed
		return nil, fmt.Errorf("failed to decode elevation PNG: %v", err)
	}
	fetchDuration := time.Since(fetchStart)
	log.Printf("Upstream fetch completed in %v: level=%d, z=%s, x=%s, y=%s", fetchDuration, seaLevel, z, x, y)

	// Start processing timer
	processStart := time.Now()

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
			blue := [4]uint8{0, 50, 120, 255}
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
						elevation := int(rVal)*256 + int(gVal) + int(bVal)/256 - 32768

						// If elevation is below the specified sea level, make it blue, otherwise transparent
						var color [4]uint8
						if elevation < seaLevel {
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
		close(ch) // Signal waiting goroutines that we failed
		return nil, fmt.Errorf("failed to encode output PNG: %v", err)
	}

	tileData := buf.Bytes()
	processDuration := time.Since(processStart)
	totalDuration := time.Since(fetchStart)

	log.Printf("Image processing completed in %v: level=%d, z=%s, x=%s, y=%s", processDuration, seaLevel, z, x, y)
	log.Printf("Total tile generation: %v (fetch: %v, process: %v): level=%d, z=%s, x=%s, y=%s",
		totalDuration, fetchDuration, processDuration, seaLevel, z, x, y)

	// Cache the result
	cache.mu.Lock()
	cache.tiles[cacheKey] = CachedTile{
		data:      tileData,
		timestamp: time.Now(),
	}
	cache.mu.Unlock()

	// Notify waiting goroutines
	ch <- tileData
	close(ch)

	log.Printf("Generated and cached tile: level=%d, z=%s, x=%s, y=%s", seaLevel, z, x, y)
	return tileData, nil
}

// serveIndex serves the index.html file
func serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

// serveTile serves a sea level tile
func serveTile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	levelStr := vars["level"]
	z := vars["z"]
	x := vars["x"]
	y := vars["y"]

	// Validate that level, z, x, y are valid integers
	level, err := strconv.Atoi(levelStr)
	if err != nil {
		http.Error(w, "Invalid sea level", http.StatusBadRequest)
		return
	}

	// Clamp sea level to valid range and 10m increments
	level = clampSeaLevel(level)

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
	tileData, err := generateSeaLevelTile(level, z, x, y)
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

	log.Printf("Served tile: level=%d, z=%s, x=%s, y=%s", level, z, x, y)
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
	r.HandleFunc("/tile/{level:-?[0-9]+}/{z:[0-9]+}/{x:[0-9]+}/{y:[0-9]+}.png", serveTile).Methods("GET")

	// Add some logging middleware
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("%s %s", r.Method, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	})

	port := "19385"
	if envPort := os.Getenv("PORT"); envPort != "" {
		port = envPort
	}

	log.Printf("Starting sea level map server on port %s", port)
	log.Printf("Visit http://localhost:%s to view the map", port)
	log.Printf("Tile endpoint: http://localhost:%s/tile/{level}/{z}/{x}/{y}.png", port)

	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
