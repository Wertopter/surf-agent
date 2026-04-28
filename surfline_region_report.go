package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const surflineBaseURL = "https://services.surfline.com/kbyg/spots/forecasts"
const surflineSpotsBaseURL = "https://services.surfline.com/kbyg/spots"
const surflineMapviewBaseURL = "https://services.surfline.com/kbyg/mapview"

// Region aliases to help quickly run common zones.
// Override with -spots if you already have your own spot IDs.
var regionSpotMap = map[string][]string{
	"north-orange-county": {"5842041f4e65fad6a770882a", "5842041f4e65fad6a770882b", "5842041f4e65fad6a7708860"},
	"south-orange-county": {"5842041f4e65fad6a77088a6", "5842041f4e65fad6a77088a7", "5842041f4e65fad6a77088ab"},
	"san-diego":           {"5842041f4e65fad6a77088b5", "5842041f4e65fad6a77088bf", "5842041f4e65fad6a77088c0"},
	"santa-cruz":          {"584204204e65fad6a7709cb6", "584204204e65fad6a7709cb8", "584204204e65fad6a7709cbc"},
}

type bounds struct {
	South float64
	West  float64
	North float64
	East  float64
}

// Approximate bounding boxes for discovery via mapview.
// These do not need to be perfect; they just define the search area.
var regionBoundsMap = map[string]bounds{
	"north-orange-county": {South: 33.50, West: -118.20, North: 33.88, East: -117.68},
	"south-orange-county": {South: 33.35, West: -118.05, North: 33.67, East: -117.45},
	"san-diego":           {South: 32.52, West: -117.40, North: 33.12, East: -116.85},
	"santa-cruz":          {South: 36.85, West: -122.30, North: 37.15, East: -121.70},
}

type waveForecastResponse struct {
	Data struct {
		Wave []struct {
			Timestamp int64 `json:"timestamp"`
			Surf      struct {
				Min float64 `json:"min"`
				Max float64 `json:"max"`
			} `json:"surf"`
			Swells []struct {
				Height    float64 `json:"height"`
				Period    float64 `json:"period"`
				Direction float64 `json:"direction"`
			} `json:"swells"`
		} `json:"wave"`
	} `json:"data"`
}

type windForecastResponse struct {
	Data struct {
		Wind []struct {
			Timestamp int64 `json:"timestamp"`
			Speed     float64
			Direction float64
			Gust      float64
		} `json:"wind"`
	} `json:"data"`
}

type tideForecastResponse struct {
	Data struct {
		Tides []struct {
			Timestamp int64   `json:"timestamp"`
			Height    float64 `json:"height"`
			Type      string  `json:"type"`
		} `json:"tides"`
	} `json:"data"`
}

type spotDetailsResponse struct {
	Data struct {
		Spot struct {
			ID   string `json:"_id"`
			Name string `json:"name"`
		} `json:"spot"`
	} `json:"data"`
}

type mapviewResponse struct {
	Data struct {
		Spots []struct {
			ID   string `json:"_id"`
			Name string `json:"name"`
		} `json:"spots"`
	} `json:"data"`
}

type spotSummary struct {
	SpotID                      string  `json:"spotId"`
	SpotName                    string  `json:"spotName"`
	AvgSurfMinFt                float64 `json:"avgSurfMinFt"`
	AvgSurfMaxFt                float64 `json:"avgSurfMaxFt"`
	AvgPrimarySwellFt           float64 `json:"avgPrimarySwellFt"`
	AvgPrimarySwellPeriodSec    float64 `json:"avgPrimarySwellPeriodSec"`
	AvgPrimarySwellDirectionDeg float64 `json:"avgPrimarySwellDirectionDeg"`
	AvgWindMph                  float64 `json:"avgWindMph"`
	AvgWindDirection            float64 `json:"avgWindDirectionDeg"`
	AvgTideFt                   float64 `json:"avgTideFt"`
	ValidHours                  int     `json:"validHours"`
}

type reportPayload struct {
	Region      string        `json:"region"`
	Source      string        `json:"source"`
	Hours       int           `json:"hours"`
	TotalSpots  int           `json:"totalSpots"`
	Spots       []spotSummary `json:"spots"`
	GeneratedAt string        `json:"generatedAt"`
}

func main() {
	region := flag.String("region", "", "Region key (e.g. san-diego). Ignored when -spots is set.")
	spotCSV := flag.String("spots", "", "Comma-separated Surfline spot IDs to analyze.")
	discover := flag.Bool("discover", true, "When -region is set, discover all spots in the region (if supported).")
	hours := flag.Int("hours", 24, "Forecast window in hours.")
	timeoutSeconds := flag.Int("timeout", 15, "HTTP timeout in seconds.")
	output := flag.String("output", "text", "Output format: text or json.")
	flag.Parse()
	outputMode := strings.ToLower(strings.TrimSpace(*output))

	if *hours <= 0 {
		exitf("hours must be > 0")
	}

	client := &http.Client{
		Timeout: time.Duration(*timeoutSeconds) * time.Second,
	}

	spotIDs, spotNames, source := resolveSpotIDs(client, *region, *spotCSV, *discover)
	if len(spotIDs) == 0 {
		exitf("no spot IDs found. pass -spots or use a known -region: %s", strings.Join(sortedRegionKeys(), ", "))
	}

	if outputMode == "text" {
		fmt.Printf("Analyzing %d spots (%s) for next %d hours...\n\n", len(spotIDs), source, *hours)
	}

	summaries := make([]spotSummary, 0, len(spotIDs))
	spotNameCache := map[string]string{}
	for id, name := range spotNames {
		spotNameCache[id] = name
	}
	for _, spotID := range spotIDs {
		sum, err := buildSpotSummary(client, spotID, *hours)
		if err != nil {
			if outputMode == "text" {
				fmt.Printf("spot %s: skipped (%v)\n", spotID, err)
			}
			continue
		}

		name, err := getSpotName(client, spotID, spotNameCache)
		if err == nil && strings.TrimSpace(name) != "" {
			sum.SpotName = name
		}

		summaries = append(summaries, sum)
	}

	if len(summaries) == 0 {
		exitf("unable to fetch usable data for all spots")
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].SpotID < summaries[j].SpotID
	})

	switch outputMode {
	case "json":
		payload := reportPayload{
			Region:      *region,
			Source:      source,
			Hours:       *hours,
			TotalSpots:  len(summaries),
			Spots:       summaries,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			exitf("failed to encode json output: %v", err)
		}
	case "text":
		for i, s := range summaries {
			rank := i + 1
			if strings.TrimSpace(s.SpotName) != "" {
				fmt.Printf("%d) Spot %s (%s)\n", rank, s.SpotID, s.SpotName)
			} else {
				fmt.Printf("%d) Spot %s\n", rank, s.SpotID)
			}
			fmt.Printf("   surf avg: %.1f-%.1fft | primary swell: %.1fft | wind: %.1fmph @ %.0fdeg | tide: %.1fft\n",
				s.AvgSurfMinFt, s.AvgSurfMaxFt, s.AvgPrimarySwellFt, s.AvgWindMph, s.AvgWindDirection, s.AvgTideFt)
			fmt.Printf("   primary swell period: %.1fs | primary swell direction: %.0fdeg\n",
				s.AvgPrimarySwellPeriodSec, s.AvgPrimarySwellDirectionDeg)
			fmt.Printf("   valid points: %d\n\n", s.ValidHours)
		}
	default:
		exitf("unknown output format %q, expected text or json", *output)
	}
}

func resolveSpotIDs(client *http.Client, region, spotCSV string, discover bool) ([]string, map[string]string, string) {
	if strings.TrimSpace(spotCSV) != "" {
		parts := strings.Split(spotCSV, ",")
		ids := make([]string, 0, len(parts))
		for _, p := range parts {
			id := strings.TrimSpace(p)
			if id != "" {
				ids = append(ids, id)
			}
		}
		return ids, map[string]string{}, "custom spots"
	}

	region = strings.TrimSpace(strings.ToLower(region))

	if discover {
		if b, ok := regionBoundsMap[region]; ok {
			refs, err := discoverSpotsByBounds(client, b)
			if err == nil && len(refs) > 0 {
				ids := make([]string, 0, len(refs))
				names := make(map[string]string, len(refs))
				for _, r := range refs {
					ids = append(ids, r.ID)
					if strings.TrimSpace(r.Name) != "" {
						names[r.ID] = r.Name
					}
				}
				return ids, names, "region discovery: " + region
			}
		}
	}

	ids, ok := regionSpotMap[region]
	if !ok {
		return nil, nil, ""
	}
	return ids, map[string]string{}, "region preset: " + region
}

func sortedRegionKeys() []string {
	keys := make([]string, 0, len(regionSpotMap))
	for k := range regionSpotMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type spotRef struct {
	ID   string
	Name string
}

func discoverSpotsByBounds(client *http.Client, b bounds) ([]spotRef, error) {
	u := buildMapviewURL(b)
	var resp mapviewResponse
	if err := fetchJSON(client, u, &resp); err != nil {
		return nil, err
	}

	seen := map[string]spotRef{}
	for _, s := range resp.Data.Spots {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			continue
		}
		seen[id] = spotRef{ID: id, Name: strings.TrimSpace(s.Name)}
	}

	out := make([]spotRef, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func buildMapviewURL(b bounds) string {
	q := url.Values{}
	q.Set("south", fmt.Sprintf("%f", b.South))
	q.Set("west", fmt.Sprintf("%f", b.West))
	q.Set("north", fmt.Sprintf("%f", b.North))
	q.Set("east", fmt.Sprintf("%f", b.East))
	return fmt.Sprintf("%s?%s", surflineMapviewBaseURL, q.Encode())
}

func buildSpotSummary(client *http.Client, spotID string, hours int) (spotSummary, error) {
	days := int(math.Ceil(float64(hours) / 24.0))
	if days < 1 {
		days = 1
	}

	waveURL := buildForecastURL("wave", spotID, days)
	windURL := buildForecastURL("wind", spotID, days)
	tideURL := buildForecastURL("tides", spotID, days)

	var waveResp waveForecastResponse
	var windResp windForecastResponse
	var tideResp tideForecastResponse

	if err := fetchJSON(client, waveURL, &waveResp); err != nil {
		return spotSummary{}, fmt.Errorf("wave fetch failed: %w", err)
	}
	if err := fetchJSON(client, windURL, &windResp); err != nil {
		return spotSummary{}, fmt.Errorf("wind fetch failed: %w", err)
	}
	if err := fetchJSON(client, tideURL, &tideResp); err != nil {
		return spotSummary{}, fmt.Errorf("tide fetch failed: %w", err)
	}

	cutoff := time.Now().Add(time.Duration(hours) * time.Hour).Unix()

	waveByTS := make(map[int64]struct {
		SurfMin, SurfMax, PrimarySwell, PrimaryPeriod, PrimaryDirection float64
	})
	for _, w := range waveResp.Data.Wave {
		if w.Timestamp > cutoff {
			continue
		}
		primary := 0.0
		primaryPeriod := 0.0
		primaryDirection := 0.0
		if len(w.Swells) > 0 {
			primary = w.Swells[0].Height
			primaryPeriod = w.Swells[0].Period
			primaryDirection = w.Swells[0].Direction
		}
		waveByTS[w.Timestamp] = struct {
			SurfMin, SurfMax, PrimarySwell, PrimaryPeriod, PrimaryDirection float64
		}{
			SurfMin:          w.Surf.Min,
			SurfMax:          w.Surf.Max,
			PrimarySwell:     primary,
			PrimaryPeriod:    primaryPeriod,
			PrimaryDirection: primaryDirection,
		}
	}

	windByTS := make(map[int64]struct {
		Speed, Direction float64
	})
	for _, w := range windResp.Data.Wind {
		if w.Timestamp > cutoff {
			continue
		}
		windByTS[w.Timestamp] = struct {
			Speed, Direction float64
		}{
			Speed:     w.Speed,
			Direction: w.Direction,
		}
	}

	tideByTS := make(map[int64]float64)
	for _, t := range tideResp.Data.Tides {
		if t.Timestamp > cutoff {
			continue
		}
		tideByTS[t.Timestamp] = t.Height
	}

	var sum spotSummary
	sum.SpotID = spotID

	for ts, wave := range waveByTS {
		wind, windOK := windByTS[ts]
		tide, tideOK := tideByTS[ts]
		if !windOK || !tideOK {
			continue
		}

		sum.AvgSurfMinFt += wave.SurfMin
		sum.AvgSurfMaxFt += wave.SurfMax
		sum.AvgPrimarySwellFt += wave.PrimarySwell
		sum.AvgPrimarySwellPeriodSec += wave.PrimaryPeriod
		sum.AvgPrimarySwellDirectionDeg += wave.PrimaryDirection
		sum.AvgWindMph += wind.Speed
		sum.AvgWindDirection += wind.Direction
		sum.AvgTideFt += tide
		sum.ValidHours++
	}

	if sum.ValidHours == 0 {
		return spotSummary{}, fmt.Errorf("no overlapping hourly points across wave/wind/tide")
	}

	count := float64(sum.ValidHours)
	sum.AvgSurfMinFt /= count
	sum.AvgSurfMaxFt /= count
	sum.AvgPrimarySwellFt /= count
	sum.AvgPrimarySwellPeriodSec /= count
	sum.AvgPrimarySwellDirectionDeg = normalizeDirectionDeg(sum.AvgPrimarySwellDirectionDeg / count)
	sum.AvgWindMph /= count
	sum.AvgWindDirection = normalizeDirectionDeg(sum.AvgWindDirection / count)
	sum.AvgTideFt /= count

	return sum, nil
}

func getSpotName(client *http.Client, spotID string, cache map[string]string) (string, error) {
	if cache != nil {
		if v, ok := cache[spotID]; ok {
			return v, nil
		}
	}

	u := buildSpotDetailsURL(spotID)
	var resp spotDetailsResponse
	if err := fetchJSON(client, u, &resp); err != nil {
		return "", err
	}

	name := strings.TrimSpace(resp.Data.Spot.Name)
	if cache != nil {
		cache[spotID] = name
	}
	return name, nil
}

func buildSpotDetailsURL(spotID string) string {
	q := url.Values{}
	q.Set("spotId", spotID)
	return fmt.Sprintf("%s/details?%s", surflineSpotsBaseURL, q.Encode())
}

func normalizeDirectionDeg(directionDeg float64) float64 {
	d := math.Mod(directionDeg, 360)
	if d < 0 {
		d += 360
	}
	return d
}

func buildForecastURL(kind, spotID string, days int) string {
	q := url.Values{}
	q.Set("spotId", spotID)
	q.Set("days", fmt.Sprintf("%d", days))
	q.Set("intervalHours", "1")
	q.Set("maxHeights", "false")
	return fmt.Sprintf("%s/%s?%s", surflineBaseURL, kind, q.Encode())
}

func fetchJSON(client *http.Client, endpoint string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", "surf-agent/1.0")
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	dec := json.NewDecoder(res.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
