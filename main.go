package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
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
	Name         string
	Realm        string
	Level        int
	ItemLevel    int
	Gold         int64
	Class        string
	Race         string
	Faction      string
	LastLogin    int64  // Unix timestamp
	ThumbnailURL string // Character portrait URL
	Professions  []Profession
	Error        string
	LastUpdate   time.Time
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
	http.HandleFunc("/api/stats", handleAPIStats)

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
		if len(acc.Characters) > 0 {
			log.Printf("  First character: name='%s', realm='%s', level=%d, class='%s'",
				acc.Characters[0].Name,
				acc.Characters[0].Realm.Slug,
				acc.Characters[0].Level,
				acc.Characters[0].PlayableClass.Name)
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
		allCharacters = append(allCharacters, stats)
	}

	// Sort characters by level (descending), then by item level (descending)
	sort.Slice(allCharacters, func(i, j int) bool {
		if allCharacters[i].Level != allCharacters[j].Level {
			return allCharacters[i].Level > allCharacters[j].Level
		}
		return allCharacters[i].ItemLevel > allCharacters[j].ItemLevel
	})

	// Calculate total gold
	var totalGold int64
	for _, char := range allCharacters {
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
		LastUpdate:      time.Now(),
		Authenticated:   true,
	}
	summaryMutex.Unlock()

	log.Printf("Data updated. Found %d characters. Total gold: %s. Mounts: %d, Pets: %d",
		len(allCharacters), formatGold(totalGold), mountsCollected, petsCollected)
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
		stats.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		log.Printf("HTTP %d for %s-%s (URL: %s)", resp.StatusCode, realm, name, profileURL)
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

	go fetchCharacterData()

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleAPIStats(w http.ResponseWriter, r *http.Request) {
	summaryMutex.RLock()
	data := accountSummary
	summaryMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

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
            
            // Check if we have the loading parameter (just came from OAuth)
            const urlParams = new URLSearchParams(window.location.search);
            const isLoading = urlParams.get('loading') === 'true';
            
            if (authenticated && hasCharacters === 0) {
                // Show message and refresh
                console.log('Waiting for character data to load...');
                setTimeout(function() {
                    console.log('Refreshing to load character data');
                    // Remove the loading parameter on refresh
                    const newUrl = window.location.pathname;
                    window.location.href = newUrl;
                }, 2000);
            } else if (isLoading && hasCharacters > 0) {
                // Data loaded! Remove loading parameter
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
            </div>
        </div>

        <div class="characters">
            {{range .Characters}}
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
        </div>

        <div class="controls">
            <form action="/refresh" method="POST" style="display: inline;">
                <button type="submit" class="btn">🔄 Refresh Data</button>
            </form>
        </div>
        {{end}}
        {{end}}
    </div>
</body>
</html>
`
