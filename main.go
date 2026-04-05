package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
)

type Config struct {
	ClientID     string
	ClientSecret string
	Region       string
	Locale       string
	Port         string
	RedirectURL  string
}

type CharacterStats struct {
	Name             string
	Realm            string
	Level            int
	ItemLevel        int
	Gold             int64
	Class            string
	Race             string
	Faction          string
	LastLogin        int64  // Unix timestamp
	ThumbnailURL     string // Character portrait URL
	Professions      []Profession
	LastRaidKill     string // Name of the most recently killed raid boss
	LastRaidInstance string // Name of the raid it was in
	LastRaidTime     int64  // Unix timestamp (ms) of the kill
	Error            string
	LastUpdate       time.Time
}

type Profession struct {
	Name string
	Tier int
	Max  int
}

type AccountSummary struct {
	Characters      []CharacterStats
	TotalGold       int64
	MountsCollected int
	PetsCollected   int
	HordeCount      int
	AllianceCount   int
	LastUpdate      time.Time
	Authenticated   bool
}

var (
	config         Config
	accountSummary AccountSummary
	summaryMutex   sync.RWMutex
	oauth2Config   *oauth2.Config
	userToken      *oauth2.Token
	tokenMutex     sync.RWMutex
	oauthState     string
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found, using environment variables")
	}

	// Initialize configuration
	port := getEnvOrDefault("PORT", "8080")
	region := getEnvOrDefault("REGION", "us")

	config = Config{
		ClientID:     os.Getenv("BLIZZARD_CLIENT_ID"),
		ClientSecret: os.Getenv("BLIZZARD_CLIENT_SECRET"),
		Region:       region,
		Locale:       getEnvOrDefault("LOCALE", "en_US"),
		Port:         port,
		RedirectURL:  fmt.Sprintf("http://localhost:%s/callback", port),
	}

	if config.ClientID == "" || config.ClientSecret == "" {
		log.Fatal("BLIZZARD_CLIENT_ID and BLIZZARD_CLIENT_SECRET are required")
	}

	// Generate random state for OAuth
	oauthState = generateRandomState()

	// Setup OAuth2 configuration
	oauth2Config = &oauth2.Config{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		RedirectURL:  config.RedirectURL,
		Scopes:       []string{"wow.profile"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  fmt.Sprintf("https://%s.battle.net/oauth/authorize", config.Region),
			TokenURL: fmt.Sprintf("https://%s.battle.net/oauth/token", config.Region),
		},
	}

	// Setup HTTP routes
	http.HandleFunc("/", handleHome)
	http.HandleFunc("/login", handleLogin)
	http.HandleFunc("/callback", handleCallback)
	http.HandleFunc("/refresh", handleRefresh)
	http.HandleFunc("/vault", handleVault)
	http.HandleFunc("/roster", handleRoster)
	http.HandleFunc("/api/stats", handleAPIStats)
	http.HandleFunc("/debug/raids", handleDebugRaids)
	http.HandleFunc("/debug/profile", handleDebugProfile)

	// Serve static files (images)
	fs := http.FileServer(http.Dir("./images"))
	http.Handle("/images/", http.StripPrefix("/images/", fs))

	// Start server
	addr := ":" + config.Port
	log.Printf("Server starting on %s", addr)
	log.Printf("Open http://localhost%s in your browser", addr)
	log.Printf("OAuth Redirect URL: %s", config.RedirectURL)
	log.Printf("Make sure this URL is configured in your Blizzard Developer Portal!")
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

func generateRandomState() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	url := oauth2Config.AuthCodeURL(oauthState)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("state") != oauthState {
		http.Error(w, "Invalid state", http.StatusBadRequest)
		return
	}

	code := r.FormValue("code")
	token, err := oauth2Config.Exchange(context.Background(), code)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to exchange token: %v", err), http.StatusInternalServerError)
		return
	}

	tokenMutex.Lock()
	userToken = token
	tokenMutex.Unlock()

	log.Println("Successfully authenticated with Battle.net")

	// Immediately set authenticated status so the page knows to wait for data
	summaryMutex.Lock()
	accountSummary.Authenticated = true
	accountSummary.Characters = nil // Clear any old data
	summaryMutex.Unlock()

	go fetchCharacterData()

	http.Redirect(w, r, "/?loading=true", http.StatusSeeOther)
}

func fetchCharacterData() {
	tokenMutex.RLock()
	token := userToken
	tokenMutex.RUnlock()

	if token == nil {
		log.Println("No authentication token available")
		summaryMutex.Lock()
		accountSummary = AccountSummary{
			Authenticated: false,
			LastUpdate:    time.Now(),
		}
		summaryMutex.Unlock()
		return
	}

	ctx := context.Background()
	client := oauth2Config.Client(ctx, token)

	// Fetch WoW account list
	accountsURL := fmt.Sprintf("https://%s.api.blizzard.com/profile/user/wow?namespace=profile-%s&locale=%s",
		config.Region, config.Region, config.Locale)

	var accountsResp struct {
		WoWAccounts []struct {
			ID         int64 `json:"id"`
			Characters []struct {
				Name  string `json:"name"`
				ID    int64  `json:"id"`
				Level int    `json:"level"`
				Realm struct {
					Slug string `json:"slug"`
					Name string `json:"name"`
				} `json:"realm"`
				PlayableClass struct {
					Name string `json:"name"`
				} `json:"playable_class"`
				PlayableRace struct {
					Name string `json:"name"`
				} `json:"playable_race"`
				Faction struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"faction"`
				ProtectedCharacter struct {
					Href string `json:"href"`
				} `json:"protected_character"`
			} `json:"characters"`
		} `json:"wow_accounts"`
	}

	resp, err := client.Get(accountsURL)
	if err != nil {
		log.Printf("Error fetching WoW accounts: %v", err)
		return
	}
	defer resp.Body.Close()

	// Decode directly into our struct
	if err := json.NewDecoder(resp.Body).Decode(&accountsResp); err != nil {
		log.Printf("Error decoding accounts response: %v", err)
		return
	}

	log.Printf("Found %d WoW accounts", len(accountsResp.WoWAccounts))
	for i, acc := range accountsResp.WoWAccounts {
		log.Printf("Account %d (ID: %d) has %d characters", i, acc.ID, len(acc.Characters))
		for _, ch := range acc.Characters {
			log.Printf("  character: name='%s', realm='%s', level=%d", ch.Name, ch.Realm.Slug, ch.Level)
		}
	}

	// Collect all characters from all WoW accounts
	var allCharacters []CharacterStats
	var wg sync.WaitGroup
	statsChan := make(chan CharacterStats, 100)

	// Rate limiting: Max 10 concurrent requests
	semaphore := make(chan struct{}, 10)

	for _, account := range accountsResp.WoWAccounts {
		for _, charData := range account.Characters {
			wg.Add(1)
			go func(cd struct {
				Name  string `json:"name"`
				ID    int64  `json:"id"`
				Level int    `json:"level"`
				Realm struct {
					Slug string `json:"slug"`
					Name string `json:"name"`
				} `json:"realm"`
				PlayableClass struct {
					Name string `json:"name"`
				} `json:"playable_class"`
				PlayableRace struct {
					Name string `json:"name"`
				} `json:"playable_race"`
				Faction struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"faction"`
				ProtectedCharacter struct {
					Href string `json:"href"`
				} `json:"protected_character"`
			}) {
				defer wg.Done()

				// Acquire semaphore
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				// Small delay to avoid rate limiting
				time.Sleep(100 * time.Millisecond)

				stats := fetchCharacterDetails(client, cd.Realm.Slug, cd.Name, cd.ProtectedCharacter.Href)
				stats.Level = cd.Level
				stats.Class = cd.PlayableClass.Name
				stats.Race = cd.PlayableRace.Name
				stats.Faction = cd.Faction.Type
				statsChan <- stats
			}(charData)
		}
	}

	go func() {
		wg.Wait()
		close(statsChan)
	}()

	for stats := range statsChan {
		// Characters below level 10 return 404 from the profile API.
		// Keep them but mark them so we can show a minimal stub card.
		if stats.Error == "HTTP 404" {
			log.Printf("Stub card for %s-%s (below level 10, no profile data available)", stats.Realm, stats.Name)
			stats.Error = "below_level_10"
		}
		allCharacters = append(allCharacters, stats)
	}

	// Sort characters by level (descending), then by item level (descending)
	sort.Slice(allCharacters, func(i, j int) bool {
		if allCharacters[i].Level != allCharacters[j].Level {
			return allCharacters[i].Level > allCharacters[j].Level
		}
		return allCharacters[i].ItemLevel > allCharacters[j].ItemLevel
	})

	// Calculate total gold and faction counts
	var totalGold int64
	var hordeCount, allianceCount int
	for _, char := range allCharacters {
		if char.Faction == "HORDE" {
			hordeCount++
		} else if char.Faction == "ALLIANCE" {
			allianceCount++
		}
		if char.Error == "" {
			totalGold += char.Gold
		}
	}

	// Fetch account-wide collections (mounts & pets)
	var mountsCollected, petsCollected int

	mountsURL := fmt.Sprintf("https://%s.api.blizzard.com/profile/user/wow/collections/mounts?namespace=profile-%s&locale=%s",
		config.Region, config.Region, config.Locale)
	if mountsResp, err := client.Get(mountsURL); err == nil {
		defer mountsResp.Body.Close()
		if mountsResp.StatusCode == 200 {
			var mountsData struct {
				Mounts []interface{} `json:"mounts"`
			}
			if err := json.NewDecoder(mountsResp.Body).Decode(&mountsData); err == nil {
				mountsCollected = len(mountsData.Mounts)
				log.Printf("Mounts collected: %d", mountsCollected)
			}
		} else {
			log.Printf("Mounts endpoint returned HTTP %d", mountsResp.StatusCode)
		}
	}

	petsURL := fmt.Sprintf("https://%s.api.blizzard.com/profile/user/wow/collections/pets?namespace=profile-%s&locale=%s",
		config.Region, config.Region, config.Locale)
	if petsResp, err := client.Get(petsURL); err == nil {
		defer petsResp.Body.Close()
		if petsResp.StatusCode == 200 {
			var petsData struct {
				Pets []interface{} `json:"pets"`
			}
			if err := json.NewDecoder(petsResp.Body).Decode(&petsData); err == nil {
				petsCollected = len(petsData.Pets)
				log.Printf("Pets collected: %d", petsCollected)
			}
		} else {
			log.Printf("Pets endpoint returned HTTP %d", petsResp.StatusCode)
		}
	}

	summaryMutex.Lock()
	accountSummary = AccountSummary{
		Characters:      allCharacters,
		TotalGold:       totalGold,
		MountsCollected: mountsCollected,
		PetsCollected:   petsCollected,
		HordeCount:      hordeCount,
		AllianceCount:   allianceCount,
		LastUpdate:      time.Now(),
		Authenticated:   true,
	}
	summaryMutex.Unlock()

	log.Printf("Data updated. Found %d characters (%d Horde, %d Alliance). Total gold: %s. Mounts: %d, Pets: %d",
		len(allCharacters), hordeCount, allianceCount, formatGold(totalGold), mountsCollected, petsCollected)
}

func fetchCharacterDetails(client *http.Client, realm, name, protectedHref string) CharacterStats {
	stats := CharacterStats{
		Name:       name,
		Realm:      realm,
		LastUpdate: time.Now(),
	}

	// Normalize name and realm to lowercase
	normalizedName := strings.ToLower(name)
	normalizedRealm := strings.ToLower(realm)

	// Fetch character profile for item level
	profileURL := fmt.Sprintf("https://%s.api.blizzard.com/profile/wow/character/%s/%s?namespace=profile-%s&locale=%s",
		config.Region, normalizedRealm, normalizedName, config.Region, config.Locale)

	log.Printf("Fetching profile: %s", profileURL)

	resp, err := client.Get(profileURL)
	if err != nil {
		stats.Error = fmt.Sprintf("API error: %v", err)
		log.Printf("Error fetching %s-%s: %v", realm, name, err)
		return stats
	}
	defer resp.Body.Close()

	// Check HTTP status
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		var errBody map[string]interface{}
		if jsonErr := json.Unmarshal(bodyBytes, &errBody); jsonErr == nil {
			log.Printf("HTTP %d for %s-%s: %v", resp.StatusCode, realm, name, errBody)
		} else {
			log.Printf("HTTP %d for %s-%s (URL: %s)", resp.StatusCode, realm, name, profileURL)
		}
		stats.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return stats
	}

	var profile map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		stats.Error = fmt.Sprintf("Decode error: %v", err)
		log.Printf("Failed to decode profile for %s-%s: %v", realm, name, err)
		return stats
	}

	// Extract average item level
	if equippedItemLevel, ok := profile["equipped_item_level"].(float64); ok {
		stats.ItemLevel = int(equippedItemLevel)
	} else if avgItemLevel, ok := profile["average_item_level"].(float64); ok {
		stats.ItemLevel = int(avgItemLevel)
	}

	// Extract last login timestamp
	if lastLogin, ok := profile["last_login_timestamp"].(float64); ok {
		stats.LastLogin = int64(lastLogin)
	}

	// Extract character thumbnail/avatar
	if media, ok := profile["media"].(map[string]interface{}); ok {
		if href, ok := media["href"].(string); ok {
			// Fetch media/avatar
			mediaResp, err := client.Get(href)
			if err == nil {
				defer mediaResp.Body.Close()
				if mediaResp.StatusCode == http.StatusOK {
					var mediaData map[string]interface{}
					if err := json.NewDecoder(mediaResp.Body).Decode(&mediaData); err == nil {
						if assets, ok := mediaData["assets"].([]interface{}); ok && len(assets) > 0 {
							// Get the avatar (usually first asset or look for key "avatar")
							for _, asset := range assets {
								if assetMap, ok := asset.(map[string]interface{}); ok {
									if key, ok := assetMap["key"].(string); ok && key == "avatar" {
										if value, ok := assetMap["value"].(string); ok {
											stats.ThumbnailURL = value
											break
										}
									}
								}
							}
							// Fallback to first asset if no avatar found
							if stats.ThumbnailURL == "" {
								if firstAsset, ok := assets[0].(map[string]interface{}); ok {
									if value, ok := firstAsset["value"].(string); ok {
										stats.ThumbnailURL = value
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Fetch professions
	professionsURL := fmt.Sprintf("https://%s.api.blizzard.com/profile/wow/character/%s/%s/professions?namespace=profile-%s&locale=%s",
		config.Region, normalizedRealm, normalizedName, config.Region, config.Locale)

	profResp, err := client.Get(professionsURL)
	if err == nil {
		defer profResp.Body.Close()
		if profResp.StatusCode == http.StatusOK {
			var profData map[string]interface{}
			if err := json.NewDecoder(profResp.Body).Decode(&profData); err == nil {
				// Primary professions
				if primaries, ok := profData["primaries"].([]interface{}); ok {
					for _, p := range primaries {
						if prof, ok := p.(map[string]interface{}); ok {
							profession := Profession{}
							if profName, ok := prof["profession"].(map[string]interface{}); ok {
								if name, ok := profName["name"].(string); ok {
									profession.Name = name
								}
							}
							// Get skill tier info (current expansion)
							if tiers, ok := prof["tiers"].([]interface{}); ok && len(tiers) > 0 {
								// Get the latest tier (usually the last one)
								latestTier := tiers[len(tiers)-1]
								if tier, ok := latestTier.(map[string]interface{}); ok {
									if skillPoints, ok := tier["skill_points"].(float64); ok {
										profession.Tier = int(skillPoints)
									}
									if maxSkillPoints, ok := tier["max_skill_points"].(float64); ok {
										profession.Max = int(maxSkillPoints)
									}
								}
							}
							if profession.Name != "" {
								stats.Professions = append(stats.Professions, profession)
							}
						}
					}
				}
			}
		}
	}

	// Try to fetch gold from the protected character endpoint
	if protectedHref != "" {
		log.Printf("Fetching protected data: %s", protectedHref)
		protectedResp, err := client.Get(protectedHref)
		if err == nil {
			defer protectedResp.Body.Close()
			if protectedResp.StatusCode == http.StatusOK {
				var protectedData map[string]interface{}
				if err := json.NewDecoder(protectedResp.Body).Decode(&protectedData); err == nil {
					// Log what's available in protected endpoint
					log.Printf("Protected character data keys for %s: %v", name, getKeys(protectedData))

					// Try to find gold/money field
					if money, ok := protectedData["money"].(float64); ok {
						stats.Gold = int64(money)
						log.Printf("Character %s-%s: Gold from protected = %s", realm, name, formatGold(int64(money)))
					} else if gold, ok := protectedData["gold"].(float64); ok {
						stats.Gold = int64(gold)
						log.Printf("Character %s-%s: Gold from protected = %s", realm, name, formatGold(int64(gold)))
					} else {
						log.Printf("Character %s-%s: No money/gold in protected endpoint", realm, name)
					}
				}
			} else {
				log.Printf("Protected endpoint returned HTTP %d for %s", protectedResp.StatusCode, name)
			}
		}
	}

	// Fetch last raid kill
	raidsURL := fmt.Sprintf("https://%s.api.blizzard.com/profile/wow/character/%s/%s/encounters/raids?namespace=profile-%s&locale=%s",
		config.Region, normalizedRealm, normalizedName, config.Region, config.Locale)
	raidsResp, err := client.Get(raidsURL)
	if err == nil {
		defer raidsResp.Body.Close()
		if raidsResp.StatusCode == http.StatusOK {
			var raidsData struct {
				Expansions []struct {
					Instances []struct {
						Instance struct {
							Name string `json:"name"`
						} `json:"instance"`
						Modes []struct {
							Progress struct {
								Encounters []struct {
									Encounter struct {
										Name string `json:"name"`
									} `json:"encounter"`
									LastKillTimestamp int64 `json:"last_kill_timestamp"`
									CompletedCount    int   `json:"completed_count"`
								} `json:"encounters"`
							} `json:"progress"`
						} `json:"modes"`
					} `json:"instances"`
				} `json:"expansions"`
			}
			if err := json.NewDecoder(raidsResp.Body).Decode(&raidsData); err == nil {
				var latestTs int64
				var latestBoss, latestInstance string
				for _, exp := range raidsData.Expansions {
					for _, inst := range exp.Instances {
						for _, mode := range inst.Modes {
							for _, enc := range mode.Progress.Encounters {
								if enc.CompletedCount > 0 && enc.LastKillTimestamp > latestTs {
									latestTs = enc.LastKillTimestamp
									latestBoss = enc.Encounter.Name
									latestInstance = inst.Instance.Name
								}
							}
						}
					}
				}
				if latestBoss != "" {
					stats.LastRaidKill = latestBoss
					stats.LastRaidInstance = latestInstance
					stats.LastRaidTime = latestTs
					log.Printf("Character %s-%s: Last raid kill = %s (%s)", realm, name, latestBoss, latestInstance)
				}
			}
		}
	}

	return stats
}

func getKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func handleHome(w http.ResponseWriter, r *http.Request) {
	summaryMutex.RLock()
	data := accountSummary
	summaryMutex.RUnlock()

	tmpl := template.Must(template.New("index").Funcs(template.FuncMap{
		"formatGold":      formatGold,
		"formatLastLogin": formatLastLogin,
		"getClassIcon":    getClassIcon,
	}).Parse(htmlTemplate))

	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Run in background so the browser isn't left waiting for all API calls to finish
	go fetchCharacterData()

	http.Redirect(w, r, "/?refreshing=true", http.StatusSeeOther)
}

func handleAPIStats(w http.ResponseWriter, r *http.Request) {
	summaryMutex.RLock()
	data := accountSummary
	summaryMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func handleDebugRaids(w http.ResponseWriter, r *http.Request) {
	tokenMutex.RLock()
	token := userToken
	tokenMutex.RUnlock()

	if token == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	// Default to axefury on the-maelstrom, allow override via query params
	realm := r.URL.Query().Get("realm")
	name := r.URL.Query().Get("name")
	if realm == "" {
		realm = "the-maelstrom"
	}
	if name == "" {
		name = "axefury"
	}

	ctx := context.Background()
	client := oauth2Config.Client(ctx, token)

	url := fmt.Sprintf("https://%s.api.blizzard.com/profile/wow/character/%s/%s/encounters/raids?namespace=profile-%s&locale=%s",
		config.Region, realm, strings.ToLower(name), config.Region, config.Locale)

	log.Printf("DEBUG raids URL: %s", url)
	resp, err := client.Get(url)
	if err != nil {
		http.Error(w, fmt.Sprintf("Request failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var raw interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		http.Error(w, fmt.Sprintf("Decode failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(raw)
}

// handleDebugProfile fetches the raw profile API response for any character
// so we can see exactly what Blizzard returns, including error bodies on 404.
// Usage: /debug/profile?realm=the-maelstrom&name=cesard
func handleDebugProfile(w http.ResponseWriter, r *http.Request) {
	tokenMutex.RLock()
	token := userToken
	tokenMutex.RUnlock()

	if token == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	realm := r.URL.Query().Get("realm")
	name := r.URL.Query().Get("name")
	if realm == "" || name == "" {
		http.Error(w, "require ?realm=X&name=Y", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	client := oauth2Config.Client(ctx, token)

	url := fmt.Sprintf("https://%s.api.blizzard.com/profile/wow/character/%s/%s?namespace=profile-%s&locale=%s",
		config.Region, strings.ToLower(realm), strings.ToLower(name), config.Region, config.Locale)

	log.Printf("DEBUG profile URL: %s", url)
	resp, err := client.Get(url)
	if err != nil {
		http.Error(w, fmt.Sprintf("Request failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	log.Printf("DEBUG profile HTTP %d for %s-%s: %s", resp.StatusCode, realm, name, string(body))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func handleVault(w http.ResponseWriter, r *http.Request) {
	summaryMutex.RLock()
	summary := accountSummary
	summaryMutex.RUnlock()

	tmpl, err := template.New("vault").Funcs(template.FuncMap{
		"lower": strings.ToLower,
	}).Parse(vaultTemplate)
	if err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, summary); err != nil {
		log.Printf("Vault template error: %v", err)
	}
}

func handleRoster(w http.ResponseWriter, r *http.Request) {
	summaryMutex.RLock()
	summary := accountSummary
	summaryMutex.RUnlock()

	tmpl, err := template.New("roster").Funcs(template.FuncMap{
		"lower": strings.ToLower,
	}).Parse(rosterTemplate)
	if err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, summary); err != nil {
		log.Printf("Roster template error: %v", err)
	}
}

var rosterTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Weekly Reset Roster</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            background: #0a0e17;
            color: #f8e6c8;
            font-family: 'Palatino Linotype', 'Book Antiqua', Palatino, serif;
            min-height: 100vh;
            padding: 20px;
            background-image: radial-gradient(ellipse at top, #1a1f2e 0%, #0a0e17 70%);
        }

        /* ── PAGE HEADER ── */
        .page-header {
            text-align: center;
            margin-bottom: 30px;
        }
        .page-header img.roster-logo {
            width: 72px;
            height: 72px;
            filter: drop-shadow(0 0 14px rgba(255, 200, 80, 0.7));
            margin-bottom: 10px;
        }
        .page-header h1 {
            font-size: 2.2em;
            color: #ffe680;
            text-shadow: 0 0 20px rgba(255, 200, 80, 0.5), 2px 2px 6px rgba(0,0,0,0.8);
            letter-spacing: 2px;
        }
        .page-header p {
            color: #9b8a6e;
            font-size: 0.95em;
            margin-top: 6px;
        }

        /* ── BACK BUTTON ── */
        .back-btn {
            display: inline-block;
            margin-bottom: 20px;
            background: linear-gradient(135deg, rgba(61,48,32,0.8), rgba(40,32,20,0.8));
            border: 2px solid #7d6a4d;
            color: #f8e6c8;
            padding: 10px 24px;
            border-radius: 6px;
            text-decoration: none;
            font-family: inherit;
            font-size: 0.95em;
            letter-spacing: 1px;
            transition: all 0.2s;
        }
        .back-btn:hover { border-color: #c8a96e; color: #ffe680; }

        /* ── QUEST LOG FRAME ── */
        .quest-log {
            max-width: 960px;
            margin: 0 auto;
            background: linear-gradient(180deg, #1c160d 0%, #120f08 100%);
            border: 3px solid #7d6a3a;
            border-radius: 8px;
            box-shadow: 0 0 40px rgba(0,0,0,0.8), inset 0 0 60px rgba(0,0,0,0.4);
            overflow: hidden;
        }
        .quest-log-header {
            background: linear-gradient(135deg, #2a1f0a, #1a130a);
            border-bottom: 2px solid #7d6a3a;
            padding: 14px 24px;
            display: flex;
            align-items: center;
            gap: 12px;
        }
        .quest-log-header h2 {
            font-size: 1.3em;
            color: #ffe680;
            letter-spacing: 2px;
            text-shadow: 0 0 10px rgba(255, 200, 80, 0.4);
        }
        .quest-log-header .header-icon {
            width: 28px;
            height: 28px;
            filter: drop-shadow(0 0 6px rgba(255,200,80,0.5));
        }
        .reset-note {
            margin-left: auto;
            font-size: 0.8em;
            color: #9b8a6e;
        }

        /* ── CHARACTER ROW ── */
        .char-row {
            border-bottom: 1px solid rgba(125, 106, 58, 0.3);
            padding: 14px 24px;
            display: grid;
            grid-template-columns: 220px 1fr;
            gap: 16px;
            align-items: start;
            transition: background 0.2s;
        }
        .char-row:last-child { border-bottom: none; }
        .char-row:hover { background: rgba(255,200,80,0.03); }

        /* completed character */
        .char-row.all-done .char-identity .char-name {
            text-decoration: line-through;
            text-decoration-color: rgba(255, 200, 80, 0.5);
            opacity: 0.5;
        }
        .char-row.all-done .char-identity .char-meta {
            opacity: 0.4;
        }

        /* ── CHARACTER IDENTITY ── */
        .char-identity {
            display: flex;
            align-items: center;
            gap: 10px;
        }
        .char-portrait {
            width: 44px;
            height: 44px;
            border-radius: 4px;
            border: 2px solid #5a4a2a;
            object-fit: cover;
            flex-shrink: 0;
        }
        .char-portrait-placeholder {
            width: 44px;
            height: 44px;
            border-radius: 4px;
            border: 2px solid #5a4a2a;
            background: rgba(90,74,42,0.3);
            flex-shrink: 0;
        }
        .char-info .char-name {
            font-size: 1.05em;
            color: #f8e6c8;
            font-weight: bold;
            transition: all 0.3s;
        }
        .char-info .char-meta {
            font-size: 0.78em;
            color: #9b8a6e;
            margin-top: 2px;
            transition: all 0.3s;
        }
        .class-icon {
            width: 20px;
            height: 20px;
            vertical-align: middle;
            margin-right: 4px;
            opacity: 0.85;
        }

        /* ── TASK LIST (quest objective style) ── */
        .task-list {
            display: flex;
            flex-direction: column;
            gap: 6px;
        }
        .task-item {
            display: flex;
            align-items: center;
            gap: 10px;
            font-size: 0.88em;
            color: #c8b48a;
            transition: all 0.3s;
        }
        .task-item.done {
            text-decoration: line-through;
            text-decoration-color: rgba(110, 231, 183, 0.6);
            color: rgba(110, 231, 183, 0.55);
        }
        .task-icon {
            width: 20px;
            height: 20px;
            flex-shrink: 0;
            opacity: 0.7;
        }
        .task-item.done .task-icon {
            opacity: 0.45;
        }
        .task-check {
            width: 16px;
            height: 16px;
            border-radius: 3px;
            border: 1px solid #5a4a2a;
            background: rgba(30,20,10,0.6);
            flex-shrink: 0;
            display: flex;
            align-items: center;
            justify-content: center;
            font-size: 11px;
            color: #6ee7b7;
            transition: all 0.2s;
        }
        .task-check.checked {
            background: rgba(110,231,183,0.15);
            border-color: #6ee7b7;
        }
        .task-label {
            flex: 1;
        }
        .task-mark-btn {
            font-size: 0.78em;
            font-family: inherit;
            margin-left: auto;
            white-space: nowrap;
            cursor: pointer;
            background: linear-gradient(135deg, rgba(61,48,32,0.9), rgba(40,32,20,0.9));
            border: 1px solid #7d6a4d;
            color: #c8b48a;
            padding: 3px 10px;
            border-radius: 4px;
            transition: all 0.15s;
        }
        .task-mark-btn:hover {
            border-color: #c8a96e;
            color: #ffe680;
            background: rgba(80,60,20,0.9);
        }
        .task-mark-btn.done-btn {
            background: rgba(110,231,183,0.12);
            border-color: #6ee7b7;
            color: #6ee7b7;
        }
        .task-mark-btn.done-btn:hover {
            background: rgba(180,50,50,0.15);
            border-color: #e06060;
            color: #ffaaaa;
        }
        .task-vault-badge {
            font-size: 0.78em;
            margin-left: auto;
            white-space: nowrap;
            color: #6ee7b7;
            opacity: 0.7;
        }
        .task-item.done .task-vault-badge {
            opacity: 0.45;
        }

        /* section header within task list */
        .task-section-label {
            font-size: 0.72em;
            letter-spacing: 2px;
            text-transform: uppercase;
            color: #7a6a4a;
            margin-top: 6px;
            margin-bottom: 2px;
            padding-bottom: 3px;
            border-bottom: 1px solid rgba(125,106,58,0.25);
        }

        /* ── SUMMARY BAR ── */
        .summary-bar {
            background: linear-gradient(135deg, #1a130a, #120f08);
            border-top: 2px solid #7d6a3a;
            padding: 12px 24px;
            display: flex;
            gap: 24px;
            align-items: center;
            font-size: 0.85em;
            color: #9b8a6e;
        }
        .summary-count {
            color: #ffe680;
            font-weight: bold;
        }
        .clear-roster-btn {
            margin-left: auto;
            background: linear-gradient(135deg, rgba(120,30,30,0.7), rgba(80,20,20,0.7));
            border: 1px solid #a04040;
            color: #ffaaaa;
            padding: 6px 18px;
            border-radius: 6px;
            cursor: pointer;
            font-family: inherit;
            font-size: 0.85em;
            letter-spacing: 1px;
            transition: all 0.2s;
        }
        .clear-roster-btn:hover { border-color: #e06060; background: rgba(160,40,40,0.8); }

        .no-chars {
            text-align: center;
            padding: 40px;
            color: #9b8a6e;
        }
    </style>
</head>
<body>
    <a href="/" class="back-btn">← Back to Characters</a>

    <div class="page-header">
        <img src="/images/quest.png" alt="Weekly Roster" class="roster-logo">
        <h1>Weekly Reset Roster</h1>
        <p>Track your weekly tasks — vault slots auto-complete when you drag characters in the Great Vault</p>
    </div>

    {{if not .Authenticated}}
    <div class="no-chars">
        <p>Please <a href="/login" style="color:#ffe680;">log in</a> to use the roster.</p>
    </div>
    {{else if eq (len .Characters) 0}}
    <div class="no-chars">
        <p>No character data yet — go back and wait for data to load.</p>
    </div>
    {{else}}

    <!-- Embed character data for JS -->
    <div id="char-data-store" style="display:none">
    {{range .Characters}}{{if ge .Level 90}}
    <span data-key="{{.Name}}-{{.Realm}}"
          data-name="{{.Name}}"
          data-realm="{{.Realm}}"
          data-class="{{lower .Class}}"
          data-thumb="{{.ThumbnailURL}}"
          data-faction="{{.Faction}}"
          data-level="{{.Level}}"></span>
    {{end}}{{end}}
    </div>

    <div class="quest-log" id="quest-log">
        <div class="quest-log-header">
            <img src="/images/quest.png" alt="Quest" class="header-icon">
            <h2>Weekly Objectives</h2>
            <span class="reset-note">Resets every Tuesday</span>
        </div>

        <!-- rows injected by JS -->
        <div id="roster-rows"></div>

        <div class="summary-bar">
            <span>Characters fully done: <span class="summary-count" id="done-count">0</span></span>
            <span>Total tracked: <span class="summary-count" id="total-count">0</span></span>
            <button class="clear-roster-btn" id="clear-manual">🗑 Clear Manual Checks</button>
        </div>
    </div>

    {{end}}

<script>
(function() {
    const VAULT_KEY   = 'wowVaultSlots';
    const MANUAL_KEY  = 'wowRosterManual';

    // ── Vault slot → label + icon mapping ──
    const SLOTS = [
        { key: 'raids-0',    icon: '/images/raid.png',    label: 'Vault: Raid Slot 1',    section: 'Raids' },
        { key: 'raids-1',    icon: '/images/raid.png',    label: 'Vault: Raid Slot 2',    section: 'Raids' },
        { key: 'raids-2',    icon: '/images/raid.png',    label: 'Vault: Raid Slot 3',    section: 'Raids' },
        { key: 'dungeons-0', icon: '/images/dungeon.png', label: 'Vault: Dungeon Slot 1', section: 'Dungeons' },
        { key: 'dungeons-1', icon: '/images/dungeon.png', label: 'Vault: Dungeon Slot 2', section: 'Dungeons' },
        { key: 'dungeons-2', icon: '/images/dungeon.png', label: 'Vault: Dungeon Slot 3', section: 'Dungeons' },
        { key: 'world-0',    icon: '/images/delve.png',   label: 'Vault: World Slot 1',   section: 'World' },
        { key: 'world-1',    icon: '/images/delve.png',   label: 'Vault: World Slot 2',   section: 'World' },
        { key: 'world-2',    icon: '/images/delve.png',   label: 'Vault: World Slot 3',   section: 'World' },
    ];

    // Extra per-character manual tasks (not vault-linked)
    const MANUAL_TASKS = [
        // ── Vault ──
        { key: 'open-vault',       icon: '/images/vault-button.png', label: 'Open your Vault',                                  section: 'Vault' },
        // ── General ──
        { key: 'world-event-quest',icon: '/images/quest.png',        label: 'World Event quest (Lady Liadrin — earn a Spark)',   section: 'General' },
        { key: 'housing-quest',    icon: '/images/quest.png',        label: 'Housing weekly (Vaeli, outside Silvermoon bank)',   section: 'General' },
        { key: 'world-boss',       icon: '/images/raid.png',         label: 'World Boss',                                       section: 'General' },
        // ── Raids ──
        { key: 'lfr-4set',         icon: '/images/raid.png',         label: 'LFR — hunt for 4-set bonus',                       section: 'Raids' },
        // ── Dungeons ──
        { key: 'mplus-weekly',     icon: '/images/dungeon.png',      label: 'M+ keys — farm highest possible (up to +10)',      section: 'Dungeons' },
        // ── World / Prey ──
        { key: 'prey-weekly',      icon: '/images/quest.png',        label: 'Nightmare Prey ×3 (weekly quest)',                 section: 'World' },
        // ── Delves ──
        { key: 'delve-t11',        icon: '/images/delve.png',        label: 'Delves — at least one T11',                        section: 'Delves' },
        // ── Currencies ──
        { key: 'spend-crests',     icon: '/images/quest.png',        label: 'Spend Champion crests & below on upgrades',        section: 'Currencies' },
        // ── Rotating / Optional ──
        { key: 'timewalking-raid', icon: '/images/raid.png',         label: 'Timewalking raid quest (Hero-track reward)',        section: 'Optional' },
        { key: 'timewalking-dung', icon: '/images/dungeon.png',      label: 'Timewalking dungeon quest (if active)',             section: 'Optional' },
    ];

    // ── Load data ──
    let vaultState  = {};
    let manualState = {};
    try { vaultState  = JSON.parse(localStorage.getItem(VAULT_KEY)  || '{}'); } catch(e) {}
    try { manualState = JSON.parse(localStorage.getItem(MANUAL_KEY) || '{}'); } catch(e) {}

    // ── Build character list from DOM ──
    const chars = [];
    document.querySelectorAll('#char-data-store span').forEach(el => {
        chars.push({
            key:     el.dataset.key,
            name:    el.dataset.name,
            realm:   el.dataset.realm,
            cls:     el.dataset.class,
            thumb:   el.dataset.thumb,
            faction: el.dataset.faction,
            level:   el.dataset.level,
        });
    });

    // ── Which vault slots is a character in? ──
    function vaultSlotsForChar(charKey) {
        const done = new Set();
        SLOTS.forEach(slot => {
            const keys = vaultState[slot.key];
            if (Array.isArray(keys) && keys.includes(charKey)) done.add(slot.key);
        });
        return done;
    }

    // ── Render ──
    function render() {
        const container = document.getElementById('roster-rows');
        container.innerHTML = '';
        let doneCount = 0;

        chars.forEach(char => {
            const vaultDone = vaultSlotsForChar(char.key);
            const manual = manualState[char.key] || {};

            // Gather all tasks and their completion status
            const allTasks = [
                ...SLOTS.map(s => ({ ...s, done: vaultDone.has(s.key), isVault: true })),
                ...MANUAL_TASKS.map(t => ({ ...t, done: !!manual[t.key], isVault: false })),
            ];
            const totalTasks = allTasks.length;
            const completedTasks = allTasks.filter(t => t.done).length;
            const allComplete = completedTasks === totalTasks;
            if (allComplete) doneCount++;

            // Build row
            const row = document.createElement('div');
            row.className = 'char-row' + (allComplete ? ' all-done' : '');
            row.dataset.charKey = char.key;

            // ── Identity cell ──
            const identity = document.createElement('div');
            identity.className = 'char-identity';

            if (char.thumb) {
                const img = document.createElement('img');
                img.className = 'char-portrait';
                img.src = char.thumb;
                img.alt = char.name;
                img.onerror = function() { this.replaceWith(makePlaceholder()); };
                identity.appendChild(img);
            } else {
                identity.appendChild(makePlaceholder());
            }

            const info = document.createElement('div');
            info.className = 'char-info';

            const nameEl = document.createElement('div');
            nameEl.className = 'char-name';
            nameEl.textContent = char.name;
            info.appendChild(nameEl);

            const metaEl = document.createElement('div');
            metaEl.className = 'char-meta';
            const clsImg = document.createElement('img');
            clsImg.className = 'class-icon';
            clsImg.src = '/images/' + char.cls + '.png';
            clsImg.alt = char.cls;
            clsImg.onerror = function() { this.style.display='none'; };
            metaEl.appendChild(clsImg);
            metaEl.appendChild(document.createTextNode(char.realm + ' · ' + completedTasks + '/' + totalTasks + ' done'));
            info.appendChild(metaEl);

            identity.appendChild(info);
            row.appendChild(identity);

            // ── Task list cell ──
            const taskList = document.createElement('div');
            taskList.className = 'task-list';

            let lastSection = null;
            allTasks.forEach(task => {
                // Section divider
                if (task.section !== lastSection) {
                    lastSection = task.section;
                    const sec = document.createElement('div');
                    sec.className = 'task-section-label';
                    sec.textContent = task.section;
                    taskList.appendChild(sec);
                }

                const item = document.createElement('div');
                item.className = 'task-item' + (task.done ? ' done' : '');

                const iconImg = document.createElement('img');
                iconImg.className = 'task-icon';
                iconImg.src = task.icon;
                iconImg.alt = '';
                iconImg.onerror = function() { this.style.display='none'; };
                item.appendChild(iconImg);

                const check = document.createElement('div');
                check.className = 'task-check' + (task.done ? ' checked' : '');
                check.textContent = task.done ? '✓' : '';
                item.appendChild(check);

                const labelEl = document.createElement('span');
                labelEl.className = 'task-label';
                labelEl.textContent = task.label;
                item.appendChild(labelEl);

                if (task.isVault) {
                    if (task.done) {
                        const badge = document.createElement('span');
                        badge.className = 'task-vault-badge';
                        badge.textContent = 'Vault ✓';
                        item.appendChild(badge);
                    }
                } else {
                    // Manual toggle button
                    const btn = document.createElement('button');
                    btn.className = 'task-mark-btn' + (task.done ? ' done-btn' : '');
                    btn.textContent = task.done ? '✓ Done — undo?' : 'Mark done';
                    btn.addEventListener('click', () => {
                        toggleManual(char.key, task.key);
                    });
                    item.appendChild(btn);
                }

                taskList.appendChild(item);
            });

            row.appendChild(taskList);
            container.appendChild(row);
        });

        document.getElementById('done-count').textContent = doneCount;
        document.getElementById('total-count').textContent = chars.length;
    }

    function makePlaceholder() {
        const d = document.createElement('div');
        d.className = 'char-portrait-placeholder';
        return d;
    }

    function toggleManual(charKey, taskKey) {
        if (!manualState[charKey]) manualState[charKey] = {};
        manualState[charKey][taskKey] = !manualState[charKey][taskKey];
        localStorage.setItem(MANUAL_KEY, JSON.stringify(manualState));
        render();
    }

    // ── Clear manual checks ──
    document.getElementById('clear-manual')?.addEventListener('click', () => {
        manualState = {};
        localStorage.setItem(MANUAL_KEY, JSON.stringify(manualState));
        render();
    });

    // ── Listen for vault changes in other tabs ──
    window.addEventListener('storage', e => {
        if (e.key === VAULT_KEY) {
            try { vaultState = JSON.parse(e.newValue || '{}'); } catch(x) {}
            render();
        }
    });

    render();
})();
</script>
</body>
</html>
`

var vaultTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Great Vault Tracker</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            background: #0a0e17;
            color: #f8e6c8;
            font-family: 'Palatino Linotype', 'Book Antiqua', Palatino, serif;
            min-height: 100vh;
            padding: 20px;
            background-image: radial-gradient(ellipse at top, #1a1f2e 0%, #0a0e17 70%);
        }
        .page-header {
            text-align: center;
            margin-bottom: 30px;
        }
        .page-header img.vault-logo {
            width: 80px;
            height: 80px;
            filter: drop-shadow(0 0 16px rgba(160, 96, 224, 0.7));
            margin-bottom: 10px;
        }
        .page-header h1 {
            font-size: 2.2em;
            color: #e8d0ff;
            text-shadow: 0 0 20px rgba(160, 96, 224, 0.6), 2px 2px 6px rgba(0,0,0,0.8);
            letter-spacing: 2px;
        }
        .page-header p {
            color: #9b8a6e;
            font-size: 0.95em;
            margin-top: 6px;
        }
        .back-btn {
            display: inline-block;
            margin-bottom: 20px;
            background: linear-gradient(135deg, rgba(61,48,32,0.8), rgba(40,32,20,0.8));
            border: 2px solid #7d6a4d;
            color: #f8e6c8;
            padding: 10px 24px;
            border-radius: 6px;
            text-decoration: none;
            font-family: inherit;
            font-size: 0.95em;
            letter-spacing: 1px;
            transition: all 0.2s;
        }
        .back-btn:hover { border-color: #9d8a6d; transform: translateY(-1px); }

        .vault-layout {
            display: flex;
            gap: 24px;
            align-items: flex-start;
            max-width: 1400px;
            margin: 0 auto;
        }

        /* ── CHARACTER ROSTER ── */
        .roster-panel {
            width: 200px;
            flex-shrink: 0;
            background: linear-gradient(135deg, rgba(26,31,46,0.9), rgba(15,20,25,0.9));
            border: 2px solid #3d3020;
            border-radius: 10px;
            padding: 16px;
            box-shadow: 0 8px 32px rgba(0,0,0,0.6);
        }
        .roster-panel h3 {
            font-size: 0.85em;
            color: #9b8a6e;
            text-transform: uppercase;
            letter-spacing: 1.5px;
            margin-bottom: 14px;
            text-align: center;
            border-bottom: 1px solid #3d3020;
            padding-bottom: 10px;
        }
        .roster-list {
            display: flex;
            flex-direction: column;
            gap: 8px;
        }
        .char-chip {
            display: flex;
            align-items: center;
            gap: 8px;
            background: rgba(61,48,32,0.5);
            border: 1px solid #5a4a30;
            border-radius: 6px;
            padding: 6px 8px;
            cursor: grab;
            transition: all 0.2s;
            user-select: none;
        }
        .char-chip:hover {
            border-color: #a060e0;
            background: rgba(80,40,120,0.4);
            transform: translateX(2px);
        }
        .char-chip.dragging {
            opacity: 0.4;
            cursor: grabbing;
        }
        .char-chip img.chip-thumb {
            width: 32px;
            height: 32px;
            border-radius: 4px;
            border: 1px solid #5a4a30;
            object-fit: cover;
            flex-shrink: 0;
        }
        .char-chip img.chip-class {
            width: 18px;
            height: 18px;
            flex-shrink: 0;
        }
        .chip-name {
            font-size: 0.82em;
            color: #f8e6c8;
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
            flex: 1;
        }
        .chip-level {
            font-size: 0.72em;
            color: #9b8a6e;
            flex-shrink: 0;
        }

        /* ── VAULT GRID ── */
        .vault-panel {
            flex: 1;
            background: linear-gradient(135deg, rgba(26,31,46,0.9), rgba(15,20,25,0.9));
            border: 2px solid #3d3020;
            border-radius: 10px;
            padding: 24px;
            box-shadow: 0 8px 32px rgba(0,0,0,0.6);
        }
        .vault-section {
            margin-bottom: 28px;
        }
        .vault-section:last-child { margin-bottom: 0; }
        .section-header {
            display: flex;
            align-items: center;
            gap: 10px;
            margin-bottom: 14px;
            padding-bottom: 10px;
            border-bottom: 1px solid #3d3020;
        }
        .section-icon { font-size: 1.4em; }
        .section-icon-img {
            width: 28px;
            height: 28px;
            object-fit: contain;
            filter: drop-shadow(0 2px 4px rgba(0,0,0,0.6));
        }
        .section-title {
            font-size: 1.1em;
            color: #f8e6c8;
            letter-spacing: 1px;
        }
        .slots-row {
            display: flex;
            gap: 14px;
            flex-wrap: wrap;
        }
        .vault-slot {
            display: flex;
            flex-direction: column;
            align-items: center;
            gap: 8px;
            flex: 1;
            min-width: 160px;
        }
        .slot-label {
            font-size: 0.72em;
            color: #9b8a6e;
            text-align: center;
            line-height: 1.3;
            min-height: 2.4em;
        }
        .slot-label span {
            color: #c8a860;
        }
        .drop-zone {
            width: 100%;
            min-height: 110px;
            border: 2px dashed #5a4a30;
            border-radius: 8px;
            display: flex;
            align-items: center;
            justify-content: flex-start;
            flex-wrap: wrap;
            gap: 6px;
            padding: 8px;
            transition: all 0.2s;
            position: relative;
            background: rgba(20,15,10,0.5);
            box-sizing: border-box;
        }
        .drop-zone.drag-over {
            border-color: #a060e0;
            background: rgba(80,40,120,0.3);
            box-shadow: 0 0 12px rgba(160,96,224,0.4);
        }
        .drop-zone.filled {
            border-style: solid;
            border-color: #7d6a4d;
        }
        .drop-zone .empty-hint {
            font-size: 0.7em;
            color: #5a4a30;
            text-align: center;
            pointer-events: none;
            width: 100%;
        }
        /* mini card inside a slot */
        .slot-card {
            width: 80px;
            border-radius: 6px;
            display: flex;
            flex-direction: column;
            align-items: center;
            justify-content: center;
            gap: 3px;
            position: relative;
            padding: 5px 4px 4px;
            background: rgba(30,25,15,0.7);
            border: 1px solid #5a4a30;
            flex-shrink: 0;
        }
        .slot-card img.sc-thumb {
            width: 44px;
            height: 44px;
            border-radius: 4px;
            border: 1px solid #7d6a4d;
            object-fit: cover;
        }
        .slot-card img.sc-class {
            position: absolute;
            bottom: 4px;
            right: 4px;
            width: 16px;
            height: 16px;
        }
        .slot-card .sc-name {
            font-size: 0.68em;
            color: #f8e6c8;
            text-align: center;
            width: 72px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        .slot-card .sc-remove {
            position: absolute;
            top: 2px;
            right: 2px;
            background: rgba(180,40,40,0.8);
            border: none;
            color: #fff;
            font-size: 0.6em;
            width: 14px;
            height: 14px;
            border-radius: 50%;
            cursor: pointer;
            display: flex;
            align-items: center;
            justify-content: center;
            line-height: 1;
            padding: 0;
        }
        .slot-card .sc-remove:hover { background: rgba(220,60,60,0.95); }

        .clear-btn {
            margin-top: 20px;
            background: linear-gradient(135deg, rgba(120,30,30,0.7), rgba(80,20,20,0.7));
            border: 1px solid #a04040;
            color: #ffaaaa;
            padding: 8px 20px;
            border-radius: 6px;
            cursor: pointer;
            font-family: inherit;
            font-size: 0.85em;
            letter-spacing: 1px;
            transition: all 0.2s;
        }
        .clear-btn:hover { border-color: #e06060; background: rgba(160,40,40,0.8); }

        .no-chars {
            text-align: center;
            padding: 40px;
            color: #9b8a6e;
        }
    </style>
</head>
<body>
    <a href="/" class="back-btn">← Back to Characters</a>

    <div class="page-header">
        <img src="/images/vault-button.png" alt="Great Vault" class="vault-logo">
        <h1>Great Vault Tracker</h1>
        <p>Drag characters onto slots to track your weekly Great Vault progress</p>
    </div>

    {{if not .Authenticated}}
    <div class="no-chars">
        <p>Please <a href="/login" style="color:#a060e0;">log in</a> to use the vault tracker.</p>
    </div>
    {{else if eq (len .Characters) 0}}
    <div class="no-chars">
        <p>No character data yet — go back and wait for data to load.</p>
    </div>
    {{else}}
    <div class="vault-layout">

        <!-- CHARACTER ROSTER -->
        <div class="roster-panel">
            <h3>Your Characters</h3>
            <div class="roster-list" id="roster">
                {{range .Characters}}
                {{if ge .Level 90}}
                <div class="char-chip"
                     draggable="true"
                     data-name="{{.Name}}"
                     data-realm="{{.Realm}}"
                     data-class="{{lower .Class}}"
                     data-level="{{.Level}}"
                     data-thumb="{{.ThumbnailURL}}"
                     data-faction="{{.Faction}}">
                    {{if .ThumbnailURL}}
                    <img class="chip-thumb" src="{{.ThumbnailURL}}" alt="{{.Name}}" onerror="this.style.display='none'">
                    {{end}}
                    <img class="chip-class" src="/images/{{lower .Class}}.png" alt="{{.Class}}" onerror="this.style.display='none'">
                    <span class="chip-name">{{.Name}}</span>
                    <span class="chip-level">{{.Level}}</span>
                </div>
                {{end}}
                {{end}}
            </div>
        </div>

        <!-- VAULT GRID -->
        <div class="vault-panel">
            <!-- RAIDS -->
            <div class="vault-section">
                <div class="section-header">
                    <img src="/images/raid.png" alt="Raids" class="section-icon-img">
                    <span class="section-title">Raids</span>
                </div>
                <div class="slots-row">
                    <div class="vault-slot">
                        <div class="slot-label">Slot 1<br><span>Defeat 2 Midnight Season 1 Boss</span></div>
                        <div class="drop-zone" data-slot="raids-0"><span class="empty-hint">Drop character</span></div>
                    </div>
                    <div class="vault-slot">
                        <div class="slot-label">Slot 2<br><span>Defeat 4 Midnight Season 1 Boss</span></div>
                        <div class="drop-zone" data-slot="raids-1"><span class="empty-hint">Drop character</span></div>
                    </div>
                    <div class="vault-slot">
                        <div class="slot-label">Slot 3<br><span>Defeat 6 Midnight Season 1 Boss</span></div>
                        <div class="drop-zone" data-slot="raids-2"><span class="empty-hint">Drop character</span></div>
                    </div>
                </div>
            </div>

            <!-- DUNGEONS -->
            <div class="vault-section">
                <div class="section-header">
                    <img src="/images/dungeon.png" alt="Dungeons" class="section-icon-img">
                    <span class="section-title">Dungeons</span>
                </div>
                <div class="slots-row">
                    <div class="vault-slot">
                        <div class="slot-label">Slot 1<br><span>Complete 1 Heroic, Mythic, or Timewalking Dungeon</span></div>
                        <div class="drop-zone" data-slot="dungeons-0"><span class="empty-hint">Drop character</span></div>
                    </div>
                    <div class="vault-slot">
                        <div class="slot-label">Slot 2<br><span>Complete 4 Heroic, Mythic, or Timewalking Dungeon</span></div>
                        <div class="drop-zone" data-slot="dungeons-1"><span class="empty-hint">Drop character</span></div>
                    </div>
                    <div class="vault-slot">
                        <div class="slot-label">Slot 3<br><span>Complete 8 Heroic, Mythic, or Timewalking Dungeon</span></div>
                        <div class="drop-zone" data-slot="dungeons-2"><span class="empty-hint">Drop character</span></div>
                    </div>
                </div>
            </div>

            <!-- WORLD -->
            <div class="vault-section">
                <div class="section-header">
                    <img src="/images/delve.png" alt="World" class="section-icon-img">
                    <span class="section-title">World (Delves / World Activities)</span>
                </div>
                <div class="slots-row">
                    <div class="vault-slot">
                        <div class="slot-label">Slot 1<br><span>Complete 2 Delves or World Activities</span></div>
                        <div class="drop-zone" data-slot="world-0"><span class="empty-hint">Drop character</span></div>
                    </div>
                    <div class="vault-slot">
                        <div class="slot-label">Slot 2<br><span>Complete 4 Delves or World Activities</span></div>
                        <div class="drop-zone" data-slot="world-1"><span class="empty-hint">Drop character</span></div>
                    </div>
                    <div class="vault-slot">
                        <div class="slot-label">Slot 3<br><span>Complete 8 Delves or World Activities</span></div>
                        <div class="drop-zone" data-slot="world-2"><span class="empty-hint">Drop character</span></div>
                    </div>
                </div>
            </div>

            <button class="clear-btn" id="clear-all">🗑 Clear All Slots</button>
        </div>
    </div>
    {{end}}

<script>
(function() {
    const STORAGE_KEY = 'wowVaultSlots';

    // Build character data map from DOM
    const charData = {};
    document.querySelectorAll('.char-chip').forEach(chip => {
        const key = chip.dataset.name + '-' + chip.dataset.realm;
        charData[key] = {
            name:    chip.dataset.name,
            realm:   chip.dataset.realm,
            cls:     chip.dataset.class,
            level:   chip.dataset.level,
            thumb:   chip.dataset.thumb,
            faction: chip.dataset.faction,
        };
    });

    let dragKey = null;

    // ── DRAG FROM ROSTER ──
    document.querySelectorAll('.char-chip').forEach(chip => {
        chip.addEventListener('dragstart', e => {
            dragKey = chip.dataset.name + '-' + chip.dataset.realm;
            chip.classList.add('dragging');
            e.dataTransfer.effectAllowed = 'copy';
        });
        chip.addEventListener('dragend', () => chip.classList.remove('dragging'));
    });

    // ── DROP ZONES ──
    document.querySelectorAll('.drop-zone').forEach(zone => {
        zone.addEventListener('dragover', e => {
            e.preventDefault();
            zone.classList.add('drag-over');
        });
        zone.addEventListener('dragleave', () => zone.classList.remove('drag-over'));
        zone.addEventListener('drop', e => {
            e.preventDefault();
            zone.classList.remove('drag-over');
            if (dragKey) {
                addToSlot(zone, dragKey);
                saveState();
            }
        });
    });

    function addToSlot(zone, key) {
        const c = charData[key];
        if (!c) return;

        // Prevent the same character appearing more than once in the same slot
        const alreadyInSlot = zone.querySelector('.slot-card[data-char-key="' + key + '"]');
        if (alreadyInSlot) return;

        // Remove empty hint if present
        const hint = zone.querySelector('.empty-hint');
        if (hint) hint.remove();

        zone.classList.add('filled');

        const card = document.createElement('div');
        card.className = 'slot-card';
        card.dataset.charKey = key;

        if (c.thumb) {
            const thumb = document.createElement('img');
            thumb.className = 'sc-thumb';
            thumb.src = c.thumb;
            thumb.alt = c.name;
            thumb.onerror = function() { this.style.display='none'; };
            card.appendChild(thumb);
        }

        const name = document.createElement('div');
        name.className = 'sc-name';
        name.textContent = c.name;
        card.appendChild(name);

        const cls = document.createElement('img');
        cls.className = 'sc-class';
        cls.src = '/images/' + c.cls + '.png';
        cls.alt = c.cls;
        cls.onerror = function() { this.style.display='none'; };
        card.appendChild(cls);

        const rm = document.createElement('button');
        rm.className = 'sc-remove';
        rm.title = 'Remove';
        rm.textContent = '✕';
        rm.addEventListener('click', () => {
            card.remove();
            if (!zone.querySelector('.slot-card')) {
                clearSlot(zone);
            }
            saveState();
        });
        card.appendChild(rm);

        zone.appendChild(card);
    }

    function clearSlot(zone) {
        zone.classList.remove('filled');
        zone.innerHTML = '<span class="empty-hint">Drop character</span>';
    }

    // ── CLEAR ALL ──
    document.getElementById('clear-all')?.addEventListener('click', () => {
        document.querySelectorAll('.drop-zone').forEach(zone => clearSlot(zone));
        saveState();
    });

    // ── PERSIST TO localStorage (array per slot) ──
    function saveState() {
        const state = {};
        document.querySelectorAll('.drop-zone').forEach(zone => {
            const keys = Array.from(zone.querySelectorAll('.slot-card')).map(c => c.dataset.charKey);
            state[zone.dataset.slot] = keys;
        });
        localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
    }

    function loadState() {
        const raw = localStorage.getItem(STORAGE_KEY);
        if (!raw) return;
        let state;
        try { state = JSON.parse(raw); } catch(e) { return; }
        document.querySelectorAll('.drop-zone').forEach(zone => {
            const keys = state[zone.dataset.slot];
            if (Array.isArray(keys)) {
                keys.forEach(key => { if (key && charData[key]) addToSlot(zone, key); });
            } else if (keys && charData[keys]) {
                // backwards-compat: old single-string format
                addToSlot(zone, keys);
            }
        });
    }

    loadState();
})();
</script>
</body>
</html>
`

func formatGold(copper int64) string {
	gold := copper / 10000
	silver := (copper % 10000) / 100
	copperRemain := copper % 100

	// Format gold with commas for better readability
	goldStr := formatWithCommas(gold)
	return fmt.Sprintf("%sg %ds %dc", goldStr, silver, copperRemain)
}

// formatWithCommas adds thousand separators to a number
func formatWithCommas(n int64) string {
	if n < 0 {
		return "-" + formatWithCommas(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return formatWithCommas(n/1000) + "," + fmt.Sprintf("%03d", n%1000)
}

// formatLastLogin formats a unix timestamp to "X days ago" or "Just now"
func formatLastLogin(timestamp int64) string {
	if timestamp == 0 {
		return "Unknown"
	}
	lastLogin := time.Unix(timestamp/1000, 0) // Convert milliseconds to seconds
	duration := time.Since(lastLogin)

	days := int(duration.Hours() / 24)
	hours := int(duration.Hours())
	minutes := int(duration.Minutes())

	if days > 365 {
		years := days / 365
		if years == 1 {
			return "1 year ago"
		}
		return fmt.Sprintf("%d years ago", years)
	} else if days > 30 {
		months := days / 30
		if months == 1 {
			return "1 month ago"
		}
		return fmt.Sprintf("%d months ago", months)
	} else if days > 0 {
		if days == 1 {
			return "Yesterday"
		}
		return fmt.Sprintf("%d days ago", days)
	} else if hours > 0 {
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	} else if minutes > 0 {
		if minutes == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", minutes)
	}
	return "Just now"
}

// getClassIcon returns the icon filename for a given class
func getClassIcon(class string) string {
	classLower := strings.ToLower(class)
	// Map class names to icon filenames
	classIcons := map[string]string{
		"death knight": "deathknight.png",
		"demon hunter": "demonhunter.png",
		"druid":        "druid.png",
		"evoker":       "evoker.png",
		"hunter":       "hunter.png",
		"mage":         "mage.png",
		"monk":         "monk.png",
		"paladin":      "paladin.png",
		"priest":       "priest.png",
		"rogue":        "rogue.png",
		"shaman":       "shaman.png",
		"warlock":      "warlock.png",
		"warrior":      "warrior.png",
	}

	if icon, ok := classIcons[classLower]; ok {
		return "/images/" + icon
	}
	return "" // Return empty if class not found
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

const htmlTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>WoW Character Stats</title>
    <style>
        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }
        body {
            font-family: 'Palatino Linotype', 'Book Antiqua', Palatino, serif;
            background: linear-gradient(to bottom, #0a0e1a 0%, #1a1f2e 50%, #0f1419 100%);
            color: #d4c5a0;
            min-height: 100vh;
            padding: 20px;
            position: relative;
        }
        body::before {
            content: '';
            position: fixed;
            top: 0;
            left: 0;
            width: 100%;
            height: 100%;
            background: url('/images/WoW_icon.svg.png') center center no-repeat;
            background-size: 800px;
            opacity: 0.03;
            z-index: -1;
            pointer-events: none;
        }
        .container {
            max-width: 1400px;
            margin: 0 auto;
        }
        header {
            text-align: center;
            margin-bottom: 10px;
            padding: 5px 20px;
            position: relative;
        }
        .github-link {
            position: absolute;
            top: 10px;
            right: 10px;
            opacity: 0.6;
            transition: opacity 0.2s ease, transform 0.2s ease;
        }
        .github-link:hover {
            opacity: 1;
            transform: scale(1.1);
        }
        .github-link svg {
            width: 28px;
            height: 28px;
            fill: #d4c5a0;
            filter: drop-shadow(0 2px 4px rgba(0,0,0,0.5));
        }
        .header-nav-btns {
            position: absolute;
            top: 10px;
            left: 10px;
            display: flex;
            flex-direction: column;
            gap: 8px;
            width: max-content;
        }
        .header-vault-btn,
        .header-roster-btn {
            padding: 10px 16px;
            font-size: 0.9em;
            display: flex;
            align-items: center;
            gap: 8px;
            white-space: nowrap;
            width: 100%;
            box-sizing: border-box;
        }
        .header-roster-btn {
            background: linear-gradient(135deg, rgba(100,80,20,0.85), rgba(70,55,10,0.85));
            border-color: #c8a020;
            color: #ffe680;
        }
        .header-roster-btn:hover {
            border-color: #ffe680;
            background: linear-gradient(135deg, rgba(130,105,30,0.9), rgba(90,70,15,0.9));
        }
        .logo {
            max-width: 350px;
            height: auto;
            margin-bottom: 5px;
            filter: drop-shadow(0 4px 8px rgba(0, 0, 0, 0.5));
        }
        h1 {
            font-size: 2.5em;
            margin-bottom: 10px;
            color: #f8e6c8;
            text-shadow: 2px 2px 8px rgba(0,0,0,0.8);
            letter-spacing: 2px;
        }
        .last-update {
            font-size: 0.9em;
            color: #9b8a6e;
            font-style: italic;
        }
        .summary {
            background: linear-gradient(135deg, rgba(26, 31, 46, 0.9) 0%, rgba(15, 20, 25, 0.9) 100%);
            border: 2px solid #3d3020;
            border-radius: 8px;
            padding: 30px;
            margin-bottom: 40px;
            box-shadow: 0 8px 32px rgba(0,0,0,0.6), inset 0 1px 0 rgba(255,255,255,0.1);
        }
        .summary-grid {
            display: flex;
            justify-content: center;
            gap: 40px;
            flex-wrap: wrap;
        }
        .summary-stat {
            text-align: center;
        }
        .summary-stat-label {
            display: flex;
            align-items: center;
            justify-content: center;
            gap: 8px;
            font-size: 0.85em;
            color: #9b8a6e;
            text-transform: uppercase;
            letter-spacing: 1.5px;
            margin-bottom: 6px;
        }
        .summary-stat-label img {
            width: 24px;
            height: 24px;
            filter: drop-shadow(0 2px 4px rgba(0,0,0,0.5));
        }
        .summary-stat-value {
            font-size: 2em;
            font-weight: bold;
            text-shadow: 2px 2px 4px rgba(0,0,0,0.8);
            letter-spacing: 1px;
        }
        .summary-stat-value.gold { color: #ffd700; text-shadow: 0 0 10px rgba(255, 215, 0, 0.5), 2px 2px 4px rgba(0,0,0,0.8); }
        .summary-stat-value.mounts { color: #a78bfa; }
        .summary-stat-value.pets { color: #6ee7b7; }
        .summary-stat-value.horde { color: #c41e3a; text-shadow: 0 0 10px rgba(196, 30, 58, 0.4), 2px 2px 4px rgba(0,0,0,0.8); }
        .summary-stat-value.alliance { color: #1a6bbf; text-shadow: 0 0 10px rgba(26, 107, 191, 0.4), 2px 2px 4px rgba(0,0,0,0.8); }
        .faction-counts {
            display: flex;
            flex-direction: column;
            gap: 6px;
            text-align: left;
        }
        .faction-count-row {
            display: flex;
            align-items: center;
            gap: 8px;
        }
        .faction-count-row img {
            width: 22px;
            height: 22px;
            filter: drop-shadow(0 2px 4px rgba(0,0,0,0.5));
        }
        .faction-count-label {
            font-size: 0.8em;
            color: #9b8a6e;
            text-transform: uppercase;
            letter-spacing: 1px;
            min-width: 60px;
        }
        .faction-count-value {
            font-size: 1.5em;
            font-weight: bold;
            text-shadow: 2px 2px 4px rgba(0,0,0,0.8);
            letter-spacing: 1px;
        }
        .summary-divider {
            width: 1px;
            background: #3d3020;
            align-self: stretch;
        }
        .total-gold {
            font-size: 2.2em;
            font-weight: bold;
            color: #ffd700;
            text-align: center;
            text-shadow: 0 0 10px rgba(255, 215, 0, 0.5), 2px 2px 4px rgba(0,0,0,0.8);
            letter-spacing: 1px;
        }
        .login-prompt {
            text-align: center;
            padding: 60px 20px;
            background: linear-gradient(135deg, rgba(26, 31, 46, 0.9) 0%, rgba(15, 20, 25, 0.9) 100%);
            border: 2px solid #3d3020;
            border-radius: 8px;
            max-width: 600px;
            margin: 0 auto;
            box-shadow: 0 8px 32px rgba(0,0,0,0.6);
        }
        .login-prompt h2 {
            margin-bottom: 20px;
            color: #f8e6c8;
            font-size: 2em;
            text-shadow: 2px 2px 6px rgba(0,0,0,0.8);
        }
        .login-prompt p {
            color: #d4c5a0;
            font-size: 1.1em;
            margin-bottom: 10px;
        }
        .characters {
            display: grid;
            grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
            gap: 24px;
            margin-bottom: 20px;
        }
        .character-card {
            background: linear-gradient(135deg, rgba(26, 31, 46, 0.95) 0%, rgba(15, 20, 25, 0.95) 100%);
            border: 2px solid #3d3020;
            border-radius: 8px;
            padding: 20px;
            box-shadow: 0 4px 16px rgba(0,0,0,0.6), inset 0 1px 0 rgba(255,255,255,0.05);
            transition: all 0.3s ease;
            position: relative;
        }
        .character-card:hover {
            transform: translateY(-5px);
            box-shadow: 0 8px 24px rgba(0,0,0,0.8), inset 0 1px 0 rgba(255,255,255,0.1);
            border-color: #5d4830;
        }
        .character-card.error {
            background: linear-gradient(135deg, rgba(60, 20, 20, 0.95) 0%, rgba(40, 15, 15, 0.95) 100%);
            border-color: #8b3a3a;
        }
        .character-card.stub {
            background: linear-gradient(135deg, rgba(30, 30, 40, 0.85) 0%, rgba(20, 20, 30, 0.85) 100%);
            border-color: #3a3a5a;
            opacity: 0.7;
        }
        .stub-notice {
            display: flex;
            flex-direction: column;
            align-items: center;
            justify-content: center;
            gap: 8px;
            padding: 18px 0 6px 0;
            color: #7a7a9a;
            font-size: 0.9em;
            text-align: center;
        }
        .stub-notice .stub-icon {
            font-size: 2em;
            opacity: 0.5;
        }
        .stub-notice .stub-text {
            color: #9a9ab8;
            font-style: italic;
        }
        .character-name {
            font-size: 1.6em;
            font-weight: bold;
            margin-bottom: 5px;
            color: #f8e6c8;
            text-shadow: 1px 1px 3px rgba(0,0,0,0.8);
            letter-spacing: 0.5px;
        }
        .character-realm {
            font-size: 0.85em;
            color: #9b8a6e;
            margin-bottom: 12px;
            font-style: italic;
        }
        .character-class {
            font-size: 0.85em;
            color: #9b8a6e;
            margin-bottom: 15px;
        }
        .faction-badge {
            display: flex;
            align-items: center;
            gap: 8px;
            padding: 8px 12px;
            border-radius: 6px;
            font-size: 0.9em;
            font-weight: bold;
            margin-bottom: 12px;
            text-transform: uppercase;
            letter-spacing: 1px;
        }
        .faction-logo {
            width: 24px;
            height: 24px;
            filter: drop-shadow(0 2px 4px rgba(0,0,0,0.5));
        }
        .faction-alliance {
            background: linear-gradient(135deg, rgba(0, 86, 184, 0.3) 0%, rgba(0, 56, 133, 0.3) 100%);
            border: 1px solid rgba(0, 112, 221, 0.6);
            color: #4da6ff;
        }
        .faction-horde {
            background: linear-gradient(135deg, rgba(149, 0, 0, 0.3) 0%, rgba(99, 0, 0, 0.3) 100%);
            border: 1px solid rgba(179, 13, 13, 0.6);
            color: #ff4444;
        }
        .race-class-info {
            display: flex;
            align-items: center;
            gap: 10px;
            font-size: 1em;
            margin-bottom: 15px;
            padding: 10px;
            background: rgba(61, 48, 32, 0.3);
            border-left: 3px solid #7d6a4d;
            border-radius: 4px;
            color: #d4c5a0;
            letter-spacing: 0.5px;
        }
        .class-icon {
            width: 32px;
            height: 32px;
            filter: drop-shadow(0 2px 4px rgba(0,0,0,0.5));
            flex-shrink: 0;
        }
        .stat-row {
            display: flex;
            justify-content: space-between;
            padding: 10px 0;
            border-bottom: 1px solid rgba(125, 106, 77, 0.2);
        }
        .stat-row:last-child {
            border-bottom: none;
        }
        .stat-label {
            color: #9b8a6e;
            font-size: 0.95em;
        }
        .stat-value {
            font-weight: bold;
            color: #f8e6c8;
            font-size: 1.05em;
        }
        .gold-value {
            color: #ffd700;
            text-shadow: 0 0 8px rgba(255, 215, 0, 0.4);
        }
        .raid-kill {
            color: #f87171;
            font-weight: 600;
        }
        .raid-instance {
            color: #94a3b8;
            font-size: 0.85em;
            font-style: italic;
        }
        .error-message {
            color: #ff6b6b;
            font-size: 0.9em;
            margin-top: 10px;
        }
        .character-header {
            display: flex;
            align-items: flex-start;
            gap: 12px;
            margin-bottom: 12px;
        }
        .character-thumbnail {
            width: 60px;
            height: 60px;
            border-radius: 6px;
            border: 2px solid #7d6a4d;
            flex-shrink: 0;
        }
        .character-info {
            flex: 1;
        }
        .professions-list {
            font-size: 0.85em;
            color: #9b8a6e;
            margin-top: 8px;
            padding: 8px;
            background: rgba(61, 48, 32, 0.2);
            border-radius: 4px;
        }
        .profession-item {
            margin: 3px 0;
        }
        .profession-none {
            font-style: italic;
            opacity: 0.6;
        }
        .last-login-info {
            font-size: 0.8em;
            color: #9b8a6e;
            margin-top: 8px;
            font-style: italic;
        }
        .controls {
            text-align: center;
            margin-top: 30px;
        }
        .refresh-note {
            margin-top: 12px;
            color: #8a7a5a;
            font-size: 0.85em;
            font-style: italic;
        }
        .refresh-note strong {
            color: #b09a6a;
        }
        .btn {
            background: linear-gradient(135deg, rgba(61, 48, 32, 0.8) 0%, rgba(40, 32, 20, 0.8) 100%);
            border: 2px solid #7d6a4d;
            color: #f8e6c8;
            padding: 14px 36px;
            font-size: 1.1em;
            font-family: 'Palatino Linotype', 'Book Antiqua', Palatino, serif;
            border-radius: 6px;
            cursor: pointer;
            transition: all 0.3s ease;
            text-decoration: none;
            display: inline-block;
            margin: 5px;
            letter-spacing: 1px;
            text-shadow: 1px 1px 2px rgba(0,0,0,0.8);
        }
        .btn:hover {
            background: linear-gradient(135deg, rgba(81, 68, 52, 0.9) 0%, rgba(60, 52, 40, 0.9) 100%);
            border-color: #9d8a6d;
            transform: translateY(-2px);
            box-shadow: 0 6px 20px rgba(0,0,0,0.6);
        }
        .btn-primary {
            background: linear-gradient(135deg, rgba(0, 86, 184, 0.8) 0%, rgba(0, 56, 133, 0.8) 100%);
            border-color: #0070dd;
            color: #fff;
        }
        .btn-primary:hover {
            background: linear-gradient(135deg, rgba(0, 106, 204, 0.9) 0%, rgba(0, 76, 153, 0.9) 100%);
            border-color: #0090ff;
        }
        .btn-vault {
            background: linear-gradient(135deg, rgba(80, 40, 120, 0.85) 0%, rgba(50, 20, 80, 0.85) 100%);
            border-color: #a060e0;
            color: #e8d0ff;
            vertical-align: middle;
        }
        .btn-vault:hover {
            background: linear-gradient(135deg, rgba(100, 60, 150, 0.95) 0%, rgba(70, 40, 110, 0.95) 100%);
            border-color: #c090ff;
        }
        .loading-message {
            text-align: center;
            padding: 50px 20px;
            font-size: 1.2em;
            color: #d4c5a0;
        }
        .spinner {
            border: 4px solid rgba(125, 106, 77, 0.3);
            border-top: 4px solid #ffd700;
            border-radius: 50%;
            width: 50px;
            height: 50px;
            animation: spin 1s linear infinite;
            margin: 20px auto;
        }
        @keyframes spin {
            0% { transform: rotate(0deg); }
            100% { transform: rotate(360deg); }
        }
    </style>
    <script>
        // Auto-refresh if authenticated but no character data loaded yet
        window.addEventListener('DOMContentLoaded', function() {
            const authenticated = {{.Authenticated}};
            const hasCharacters = {{if .Characters}}{{len .Characters}}{{else}}0{{end}};
            
            const urlParams = new URLSearchParams(window.location.search);
            const isLoading = urlParams.get('loading') === 'true';
            const isRefreshing = urlParams.get('refreshing') === 'true';

            if (isRefreshing) {
                // Show a banner and poll until LastUpdate changes
                const banner = document.createElement('div');
                banner.id = 'refresh-banner';
                banner.style.cssText = 'position:fixed;top:0;left:0;right:0;background:#1a3a1a;color:#6ee7b7;text-align:center;padding:10px;font-size:0.95em;z-index:9999;border-bottom:1px solid #2d5a2d;';
                banner.textContent = '⏳ Fetching latest data from Battle.net… page will update automatically.';
                document.body.prepend(banner);

                const lastUpdate = '{{.LastUpdate}}';
                const poll = setInterval(function() {
                    fetch('/api/stats')
                        .then(r => r.json())
                        .then(data => {
                            if (data.LastUpdate && data.LastUpdate !== lastUpdate) {
                                clearInterval(poll);
                                window.location.href = '/';
                            }
                        })
                        .catch(() => {});
                }, 3000);
            } else if (authenticated && hasCharacters === 0) {
                // Just came from OAuth, waiting for first load
                console.log('Waiting for character data to load...');
                setTimeout(function() {
                    const newUrl = window.location.pathname;
                    window.location.href = newUrl;
                }, 2000);
            } else if (isLoading && hasCharacters > 0) {
                const newUrl = window.location.pathname;
                window.history.replaceState({}, document.title, newUrl);
            }
        });
    </script>
</head>
<body>
    <div class="container">
        <header>
            <a href="https://github.com/MichaelCade/wow_stats" target="_blank" rel="noopener" class="github-link" title="View on GitHub">
                <svg viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/></svg>
            </a>
            {{if .Authenticated}}
            <div class="header-nav-btns">
                <a href="/vault" class="btn btn-vault header-vault-btn">
                    <img src="/images/vault-button.png" alt="Vault" style="width:22px;height:22px;flex-shrink:0;filter:drop-shadow(0 1px 3px rgba(0,0,0,0.6));">
                    Great Vault
                </a>
                <a href="/roster" class="btn btn-vault header-roster-btn">
                    <img src="/images/quest.png" alt="Roster" style="width:22px;height:22px;flex-shrink:0;filter:drop-shadow(0 1px 3px rgba(0,0,0,0.6));">
                    Weekly Roster
                </a>
            </div>
            {{end}}
            <img src="/images/World-of-Warcraft-Logo-2001.png" alt="World of Warcraft" class="logo">
            {{if .Authenticated}}
            <div class="last-update">Last Updated: {{.LastUpdate.Format "Jan 02, 2006 15:04:05 MST"}}</div>
            {{end}}
        </header>

        {{if not .Authenticated}}
        <div class="login-prompt">
            <h2>Welcome to WoW Stats Tracker!</h2>
            <p>Please login with your Battle.net account to view your characters.</p>
            <br>
            <a href="/login" class="btn btn-primary">🔐 Login with Battle.net</a>
        </div>
        {{else}}
        {{if eq (len .Characters) 0}}
        <div class="loading-message">
            <div class="spinner"></div>
            <p>Loading your character data from Battle.net...</p>
            <p style="opacity: 0.7; margin-top: 10px;">This page will refresh automatically.</p>
        </div>
        {{else}}
        <div class="summary">
            <div class="summary-grid">
                <div class="summary-stat">
                    <div class="summary-stat-label">
                        <img src="/images/gold.png" alt="Gold"> Total Account Gold
                    </div>
                    <div class="summary-stat-value gold">{{formatGold .TotalGold}}</div>
                </div>
                {{if .MountsCollected}}
                <div class="summary-divider"></div>
                <div class="summary-stat">
                    <div class="summary-stat-label">
                        <img src="/images/mount.png" alt="Mounts"> Mounts Collected
                    </div>
                    <div class="summary-stat-value mounts">{{.MountsCollected}}</div>
                </div>
                {{end}}
                {{if .PetsCollected}}
                <div class="summary-divider"></div>
                <div class="summary-stat">
                    <div class="summary-stat-label">
                        <img src="/images/pet.png" alt="Pets"> Pets Collected
                    </div>
                    <div class="summary-stat-value pets">{{.PetsCollected}}</div>
                </div>
                {{end}}
                {{if or .HordeCount .AllianceCount}}
                <div class="summary-divider"></div>
                <div class="summary-stat">
                    <div class="summary-stat-label" style="margin-bottom: 10px;">Characters by Faction</div>
                    <div class="faction-counts">
                        {{if .AllianceCount}}
                        <div class="faction-count-row">
                            <img src="/images/wow-alliance.png" alt="Alliance">
                            <span class="faction-count-label">Alliance</span>
                            <span class="faction-count-value alliance">{{.AllianceCount}}</span>
                        </div>
                        {{end}}
                        {{if .HordeCount}}
                        <div class="faction-count-row">
                            <img src="/images/wow-horde.png" alt="Horde">
                            <span class="faction-count-label">Horde</span>
                            <span class="faction-count-value horde">{{.HordeCount}}</span>
                        </div>
                        {{end}}
                    </div>
                </div>
                {{end}}
            </div>
        </div>

        <div class="characters">
            {{range .Characters}}
            {{if eq .Error "below_level_10"}}
            <div class="character-card stub">
                <div class="character-header">
                    <div class="character-info">
                        <div class="character-name">{{.Name}}</div>
                        <div class="character-realm">{{.Realm}}</div>
                    </div>
                </div>
                {{if eq .Faction "ALLIANCE"}}
                <div class="faction-badge faction-alliance">
                    <img src="/images/wow-alliance.png" alt="Alliance" class="faction-logo">
                    Alliance
                </div>
                {{else if eq .Faction "HORDE"}}
                <div class="faction-badge faction-horde">
                    <img src="/images/wow-horde.png" alt="Horde" class="faction-logo">
                    Horde
                </div>
                {{end}}
                {{if .Class}}
                <div class="race-class-info">
                    {{if getClassIcon .Class}}
                    <img src="{{getClassIcon .Class}}" alt="{{.Class}}" class="class-icon">
                    {{end}}
                    <span>{{.Race}} {{.Class}}</span>
                </div>
                {{end}}
                <div class="stat-row">
                    <span class="stat-label">Level:</span>
                    <span class="stat-value">{{.Level}}</span>
                </div>
                <div class="stub-notice">
                    <span class="stub-icon">🌱</span>
                    <span class="stub-text">Below level 10 — limited profile data</span>
                </div>
            </div>
            {{else}}
            <div class="character-card {{if .Error}}error{{end}}">
                {{if not .Error}}
                <div class="character-header">
                    {{if .ThumbnailURL}}
                    <img src="{{.ThumbnailURL}}" alt="{{.Name}}" class="character-thumbnail">
                    {{end}}
                    <div class="character-info">
                        <div class="character-name">{{.Name}}</div>
                        <div class="character-realm">{{.Realm}}</div>
                    </div>
                </div>
                {{else}}
                <div class="character-name">{{.Name}}</div>
                <div class="character-realm">{{.Realm}}</div>
                {{end}}
                
                {{if not .Error}}
                    {{if eq .Faction "ALLIANCE"}}
                    <div class="faction-badge faction-alliance">
                        <img src="/images/wow-alliance.png" alt="Alliance" class="faction-logo">
                        Alliance
                    </div>
                    {{else if eq .Faction "HORDE"}}
                    <div class="faction-badge faction-horde">
                        <img src="/images/wow-horde.png" alt="Horde" class="faction-logo">
                        Horde
                    </div>
                    {{end}}
                    <div class="race-class-info">
                        {{if getClassIcon .Class}}
                        <img src="{{getClassIcon .Class}}" alt="{{.Class}}" class="class-icon">
                        {{end}}
                        <span>{{.Race}} {{.Class}}</span>
                    </div>
                {{else}}
                    <div class="character-class">{{.Class}}</div>
                {{end}}
                {{if .Error}}
                    <div class="error-message">❌ {{.Error}}</div>
                {{else}}
                    <div class="stat-row">
                        <span class="stat-label">Level:</span>
                        <span class="stat-value">{{.Level}}</span>
                    </div>
                    <div class="stat-row">
                        <span class="stat-label">Item Level:</span>
                        <span class="stat-value">{{.ItemLevel}}</span>
                    </div>
                    <div class="stat-row">
                        <span class="stat-label">Gold:</span>
                        <span class="stat-value gold-value">{{formatGold .Gold}}</span>
                    </div>
                    {{if .LastLogin}}
                    <div class="stat-row">
                        <span class="stat-label">Last Played:</span>
                        <span class="stat-value">{{formatLastLogin .LastLogin}}</span>
                    </div>
                    {{end}}
                    {{if .LastRaidKill}}
                    <div class="stat-row">
                        <span class="stat-label">Last Raid Kill:</span>
                        <span class="stat-value raid-kill">{{.LastRaidKill}}</span>
                    </div>
                    <div class="stat-row">
                        <span class="stat-label"></span>
                        <span class="stat-value raid-instance">{{.LastRaidInstance}} · {{formatLastLogin .LastRaidTime}}</span>
                    </div>
                    {{end}}
                    {{if .Professions}}
                    <div class="professions-list">
                        <strong>Professions:</strong><br>
                        {{range .Professions}}
                        <div class="profession-item">• {{.Name}} {{if .Max}}({{.Tier}}/{{.Max}}){{end}}</div>
                        {{end}}
                    </div>
                    {{else}}
                    <div class="professions-list">
                        <div class="profession-none">No professions selected</div>
                    </div>
                    {{end}}
                {{end}}
            </div>
            {{end}}
            {{end}}
        </div>

        <div class="controls">
            <form action="/refresh" method="POST" style="display: inline;">
                <button type="submit" class="btn" id="refresh-btn">🔄 Refresh Data</button>
            </form>
            <div class="refresh-note">
                💡 Data only updates after you <strong>log out</strong> of a character in-game — Blizzard's API reflects the last logout state.
            </div>
        </div>
        {{end}}
        {{end}}
    </div>
</body>
</html>
`
