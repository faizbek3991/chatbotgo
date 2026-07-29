package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ToolFunc executes a tool given Gemini's requested arguments and returns a
// JSON-serializable result to send back to the model.
type ToolFunc func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error)

// Declarations describes the tools available to Gemini, in the schema it
// expects. Keep descriptions specific — the model chooses whether and how
// to call a tool based on these strings alone.
func Declarations() []FunctionDeclaration {
	return []FunctionDeclaration{
		{
			Name:        "get_weather",
			Description: "Get the current temperature (Celsius) and windspeed (km/h) for a location given its latitude and longitude.",
			Parameters: &Schema{
				Type: "OBJECT",
				Properties: map[string]*Schema{
					"latitude":  {Type: "NUMBER", Description: "Latitude of the location, e.g. 3.1390"},
					"longitude": {Type: "NUMBER", Description: "Longitude of the location, e.g. 101.6869"},
				},
				Required: []string{"latitude", "longitude"},
			},
		},
		{
			Name:        "get_exchange_rate",
			Description: "Get the current exchange rate between two ISO 4217 currency codes, e.g. USD to MYR.",
			Parameters: &Schema{
				Type: "OBJECT",
				Properties: map[string]*Schema{
					"from": {Type: "STRING", Description: "3-letter source currency code, e.g. USD"},
					"to":   {Type: "STRING", Description: "3-letter target currency code, e.g. MYR"},
				},
				Required: []string{"from", "to"},
			},
		},
		{
			Name:        "eval_math",
			Description: "Evaluate a basic arithmetic expression such as (12 + 8) * 3 / 2. Supports + - * / ^ and parentheses.",
			Parameters: &Schema{
				Type: "OBJECT",
				Properties: map[string]*Schema{
					"expression": {Type: "STRING", Description: "The arithmetic expression to evaluate"},
				},
				Required: []string{"expression"},
			},
		},
		{
			Name:        "get_prayer_times",
			Description: "Get today's Islamic prayer times (Imsak, Subuh/Fajr, Syuruk, Dhuha, Zohor/Dhuhr, Asar, Maghrib, Isyak) for a Malaysian state or district, sourced from JAKIM.",
			Parameters: &Schema{
				Type: "OBJECT",
				Properties: map[string]*Schema{
					"location": {Type: "STRING", Description: "A Malaysian state or district name, e.g. 'Selangor', 'Petaling Jaya', 'Kuala Lumpur'"},
				},
				Required: []string{"location"},
			},
		},
		{
			Name:        "get_fuel_price",
			Description: "Get this week's official Malaysian retail fuel prices (RON95, RON97, diesel) per litre, including subsidized rates, from the government's open data portal.",
			Parameters:  &Schema{Type: "OBJECT"},
		},
		{
			Name:        "get_public_holidays",
			Description: "Get Malaysian public holidays for a year, optionally filtered to one state.",
			Parameters: &Schema{
				Type: "OBJECT",
				Properties: map[string]*Schema{
					"state": {Type: "STRING", Description: "Optional Malaysian state name, e.g. 'Selangor'. Omit for all states."},
					"year":  {Type: "NUMBER", Description: "Optional year, e.g. 2026. Defaults to the current year if omitted."},
				},
			},
		},
	}
}

// Registry wires each declared tool name to its Go implementation.
func Registry() map[string]ToolFunc {
	return map[string]ToolFunc{
		"get_weather":         getWeather,
		"get_exchange_rate":   getExchangeRate,
		"eval_math":           evalMath,
		"get_prayer_times":    getPrayerTimes,
		"get_fuel_price":      getFuelPrice,
		"get_public_holidays": getPublicHolidays,
	}
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// getWeather calls Open-Meteo, a free, no-API-key weather API.
func getWeather(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	lat, ok1 := args["latitude"].(float64)
	lon, ok2 := args["longitude"].(float64)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("latitude and longitude are required numbers")
	}

	u := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%s&longitude=%s&current_weather=true",
		strconv.FormatFloat(lat, 'f', 4, 64),
		strconv.FormatFloat(lon, 'f', 4, 64),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		CurrentWeather struct {
			Temperature float64 `json:"temperature"`
			Windspeed   float64 `json:"windspeed"`
		} `json:"current_weather"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse open-meteo response: %w", err)
	}

	return map[string]interface{}{
		"temperature_celsius": parsed.CurrentWeather.Temperature,
		"windspeed_kmh":       parsed.CurrentWeather.Windspeed,
	}, nil
}

// getExchangeRate calls frankfurter.app, a free, no-API-key currency API.
func getExchangeRate(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	from, _ := args["from"].(string)
	to, _ := args["to"].(string)
	if from == "" || to == "" {
		return nil, fmt.Errorf("from and to currency codes are required")
	}

	u := fmt.Sprintf("https://api.frankfurter.app/latest?from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse frankfurter response: %w", err)
	}

	rate, ok := parsed.Rates[to]
	if !ok {
		return nil, fmt.Errorf("no rate found for %s -> %s", from, to)
	}

	return map[string]interface{}{
		"from": from,
		"to":   to,
		"rate": rate,
	}, nil
}

// evalMath uses the hand-rolled recursive-descent parser in mathexpr.go —
// no external dependency needed for a demo-scale calculator.
func evalMath(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	expr, _ := args["expression"].(string)
	if expr == "" {
		return nil, fmt.Errorf("expression is required")
	}
	result, err := evaluateExpression(expr)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"expression": expr,
		"result":     result,
	}, nil
}

// myt is Malaysia Time — a fixed UTC+8 offset, since Malaysia has observed
// no daylight saving since 1982. Using a fixed zone (rather than
// time.LoadLocation) avoids depending on an IANA tzdata install being
// present on the host.
var myt = time.FixedZone("MYT", 8*60*60)

// getPrayerTimes calls api.waktusolat.app, a free, no-API-key prayer times
// API sourced from JAKIM. It resolves a free-text location to a JAKIM zone
// code via the /zones list rather than trusting the model to know exact
// zone codes.
func getPrayerTimes(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	location, _ := args["location"].(string)
	if location == "" {
		return nil, fmt.Errorf("location is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.waktusolat.app/zones", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var zones []struct {
		JakimCode string `json:"jakimCode"`
		Negeri    string `json:"negeri"`
		Daerah    string `json:"daerah"`
	}
	if err := json.Unmarshal(body, &zones); err != nil {
		return nil, fmt.Errorf("parse zones response: %w", err)
	}

	needle := strings.ToLower(location)
	var zone, negeri, daerah string
	for _, z := range zones {
		if strings.Contains(strings.ToLower(z.Negeri), needle) || strings.Contains(strings.ToLower(z.Daerah), needle) {
			zone, negeri, daerah = z.JakimCode, z.Negeri, z.Daerah
			break
		}
	}
	if zone == "" {
		return nil, fmt.Errorf("no JAKIM zone found matching location %q", location)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.waktusolat.app/v2/solat/"+zone, nil)
	if err != nil {
		return nil, err
	}
	resp, err = httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var solat struct {
		Prayers []struct {
			Day     int   `json:"day"`
			Imsak   int64 `json:"imsak"`
			Fajr    int64 `json:"fajr"`
			Syuruk  int64 `json:"syuruk"`
			Dhuha   int64 `json:"dhuha"`
			Dhuhr   int64 `json:"dhuhr"`
			Asr     int64 `json:"asr"`
			Maghrib int64 `json:"maghrib"`
			Isha    int64 `json:"isha"`
		} `json:"prayers"`
	}
	if err := json.Unmarshal(body, &solat); err != nil {
		return nil, fmt.Errorf("parse solat response: %w", err)
	}

	today := time.Now().In(myt)
	for _, p := range solat.Prayers {
		if p.Day != today.Day() {
			continue
		}
		fmtTime := func(ts int64) string { return time.Unix(ts, 0).In(myt).Format("15:04") }
		return map[string]interface{}{
			"zone":    zone,
			"negeri":  negeri,
			"daerah":  daerah,
			"date":    today.Format("2006-01-02"),
			"imsak":   fmtTime(p.Imsak),
			"fajr":    fmtTime(p.Fajr),
			"syuruk":  fmtTime(p.Syuruk),
			"dhuha":   fmtTime(p.Dhuha),
			"dhuhr":   fmtTime(p.Dhuhr),
			"asr":     fmtTime(p.Asr),
			"maghrib": fmtTime(p.Maghrib),
			"isha":    fmtTime(p.Isha),
		}, nil
	}
	return nil, fmt.Errorf("no prayer time entry found for today in zone %s", zone)
}

// getFuelPrice calls api.data.gov.my's data-catalogue API, a free,
// no-API-key open data endpoint, for the latest weekly retail fuel prices.
func getFuelPrice(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.data.gov.my/data-catalogue/?id=fuelprice&limit=1&sort=-date", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var records []struct {
		Date        string  `json:"date"`
		RON95       float64 `json:"ron95"`
		RON97       float64 `json:"ron97"`
		Diesel      float64 `json:"diesel"`
		RON95Budi95 float64 `json:"ron95_budi95"`
		DieselBudi  float64 `json:"diesel_budi"`
	}
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("parse fuelprice response: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("no fuel price data returned")
	}

	r := records[0]
	return map[string]interface{}{
		"date":                  r.Date,
		"ron95_myr_per_litre":   r.RON95,
		"ron97_myr_per_litre":   r.RON97,
		"diesel_myr_per_litre":  r.Diesel,
		"ron95_subsidized_myr":  r.RON95Budi95,
		"diesel_subsidized_myr": r.DieselBudi,
	}, nil
}

// malaysiaStateCodes maps common Malaysian state/federal-territory names to
// the 3-letter codes used by the public holidays API. This list is fixed —
// Malaysia's 13 states + 3 federal territories don't change — so it's
// hardcoded rather than fetched, unlike the more granular JAKIM zones.
var malaysiaStateCodes = map[string]string{
	"johor":           "JHR",
	"kedah":           "KDH",
	"kelantan":        "KTN",
	"melaka":          "MLK",
	"malacca":         "MLK",
	"negeri sembilan": "NSN",
	"pahang":          "PHG",
	"perak":           "PRK",
	"perlis":          "PLS",
	"pulau pinang":    "PNG",
	"penang":          "PNG",
	"sabah":           "SBH",
	"sarawak":         "SWK",
	"selangor":        "SGR",
	"terengganu":      "TRG",
	"kuala lumpur":    "KUL",
	"kl":              "KUL",
	"labuan":          "LBN",
	"putrajaya":       "PJY",
}

// getPublicHolidays calls malaysia-holiday.dydxsoft.my, a free, no-API-key
// public holidays API, optionally filtered to one state.
func getPublicHolidays(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	year := time.Now().Year()
	if y, ok := args["year"].(float64); ok && y != 0 {
		year = int(y)
	}

	state, _ := args["state"].(string)
	var stateCode string
	if state != "" {
		stateCode = malaysiaStateCodes[strings.ToLower(strings.TrimSpace(state))]
		if stateCode == "" {
			return nil, fmt.Errorf("unrecognized Malaysian state %q", state)
		}
	}

	u := fmt.Sprintf("https://malaysia-holiday.dydxsoft.my/api/v1/holidays?year=%d", year)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Data []struct {
			Name       string   `json:"name"`
			Date       string   `json:"date"`
			DayName    string   `json:"day_name"`
			StateCodes []string `json:"state_codes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse holidays response: %w", err)
	}

	holidays := make([]map[string]interface{}, 0, len(parsed.Data))
	for _, h := range parsed.Data {
		if stateCode != "" {
			found := false
			for _, sc := range h.StateCodes {
				if sc == stateCode {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		holidays = append(holidays, map[string]interface{}{
			"name":     h.Name,
			"date":     h.Date,
			"day_name": h.DayName,
		})
	}

	return map[string]interface{}{
		"year":     year,
		"state":    state,
		"holidays": holidays,
	}, nil
}
