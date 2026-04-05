# WoW Character Stats Tracker

A self-hosted World of Warcraft character dashboard that uses the official Battle.net API to display stats and track weekly activities across all characters on your account.

![WoW Stats Tracker Preview](example.png)

## Features

### � Home — Character Dashboard
- **All Characters** — Automatically discovers every character on your account
- **Account Summary** — Total gold, mount count, pet count, and Horde/Alliance breakdown at a glance
- **Character Cards** — Portrait, level, item level, race, class (with icon), faction, and last played timestamp
- **Gold Tracking** — Per-character gold and formatted total account gold
- **Sorted Display** — Characters ordered by level then item level (descending)
- **Raid Progress** — Current tier raid progress (Normal / Heroic / Mythic) per character
- **Dungeon & Delve Info** — Mythic+ and Delve completion tracking per character

### 🔒 Great Vault (`/vault`)
- **Drag-and-drop interface** — Drag characters from your roster onto vault slots
- **Three activity tracks** — Raid (3 slots), Mythic+ Dungeons (3 slots), Delves (3 slots)
- **Persistent state** — Slot assignments saved to `localStorage` and survive page refreshes
- **Cross-tab sync** — Vault state updates instantly across multiple open tabs
- **Visual feedback** — Character portrait and class icon shown in each filled slot

### 📋 Weekly Roster (`/roster`)
- **Quest-log style layout** — Weekly tasks per character in a scrollable list
- **Task tracking** — Great Vault slots, raid bosses, dungeons, and delves
- **Manual check-offs** — Mark tasks complete with a click; state persists in `localStorage`
- **Vault integration** — Characters dragged into the Great Vault page auto-complete their vault tasks here
- **Warband view** — All eligible characters (level 90+) shown in one place

### ⚒️ Profession Tracker (`/professions`)
- **Full profession matrix** — All characters as columns, all professions as rows
- **Three tabs** — Primary Professions / Secondary Professions / All Tiers (expansion history)
- **Skill bars** — Visual progress bar per expansion tier with the tier name shown on hover
- **Maxed highlighting** — Skill bars turn gold when a profession is maxed
- **All expansion tiers** — Shows every expansion's profession tier, not just current
- **Secondary professions** — Cooking, Fishing, Archaeology and other secondaries tracked separately

### 🌐 General
- **Battle.net OAuth2** — Secure login; no passwords stored
- **Parallel API fetching** — All character data fetched concurrently for fast load times
- **Auto-refresh** — Page polls until character data is ready after first login
- **Consistent navigation** — Shared header with logo and colour-coded nav buttons on every page
- **Multi-region support** — US, EU, KR, TW regions configurable via environment variable

---

## Prerequisites

- [Go 1.22+](https://golang.org/dl/) **or** [Docker](https://docs.docker.com/get-docker/)
- A [Battle.net Developer Account](https://develop.battle.net/)

---

## Step 1 — Create a Battle.net API Client

1. Go to [https://develop.battle.net/access/](https://develop.battle.net/access/) and log in with your Battle.net account
2. Click **Create Client**
3. Fill in the details:
   - **Client Name**: anything you like (e.g. `WoW Stats Tracker`)
   - **Redirect URLs**: `http://localhost:8081/callback`
   - **Service URL**: `http://localhost:8081`
4. Click **Save** — you will be given a **Client ID** and **Client Secret**

> ⚠️ Keep your Client Secret private — never commit it to a public repository.

---

## Step 2 — Configure the Application

Copy the example environment file and fill in your credentials:

```bash
cp .env.example .env
```

Edit `.env`:

```env
BLIZZARD_CLIENT_ID=your_client_id_here
BLIZZARD_CLIENT_SECRET=your_client_secret_here

REGION=eu        # us, eu, kr, tw
LOCALE=en_GB     # en_US, en_GB, de_DE, fr_FR, es_ES, etc.
PORT=8081
```

---

## Step 3 — Run the Application

### Option A: Go (local)

```bash
go run main.go
```

### Option B: Docker Compose

```bash
docker-compose up --build
```

Then open [http://localhost:8081](http://localhost:8081) in your browser and click **Login with Battle.net**.

---

## Pages & Routes

| Route | Description |
|-------|-------------|
| `/` | Home — character dashboard with account summary |
| `/vault` | Great Vault tracker — drag-and-drop weekly slot planner |
| `/roster` | Weekly Roster — per-character weekly task checklist |
| `/professions` | Profession Tracker — cross-character profession skill matrix |
| `/refresh` | Force re-fetch of all character data from Battle.net |

---

## How It Works

1. You click **Login with Battle.net** — you are redirected to Blizzard's OAuth page
2. After authorising, the app fetches all characters on your account automatically
3. For each character it calls multiple API endpoints in parallel:
   - Public profile (level, item level, race, class, faction, last login)
   - Media API (character portrait/avatar)
   - Protected character endpoint (gold — requires OAuth)
   - Profession data (all expansion tiers, primary and secondary)
   - Raid progress (current tier, all difficulties)
   - Mythic+ and Delve activity data
4. Results are displayed sorted by level, then item level
5. The Great Vault and Weekly Roster use `localStorage` — no server-side storage required

---

## Security Notes

- Your `.env` file is listed in `.gitignore` and will **never** be committed
- Credentials are loaded from environment variables only — nothing is hardcoded
- The OAuth token is held in memory only and is not persisted to disk
- No database or external storage is used — all state is stored client-side in `localStorage`

---

## Region & Locale Reference

| Region   | Code | Example Locales                          |
|----------|------|------------------------------------------|
| Europe   | `eu` | `en_GB`, `de_DE`, `fr_FR`, `es_ES`, `ru_RU` |
| Americas | `us` | `en_US`, `es_MX`, `pt_BR`               |
| Korea    | `kr` | `ko_KR`                                  |
| Taiwan   | `tw` | `zh_TW`                                  |

---

## Troubleshooting

**"BLIZZARD_CLIENT_ID and BLIZZARD_CLIENT_SECRET are required"**
→ Make sure your `.env` file exists and has your credentials filled in.

**Characters not showing after login**
→ The app fetches data in the background after OAuth. Wait a few seconds — the page will refresh automatically.

**404 errors for some characters in logs**
→ These are deleted or renamed characters still listed in your account. This is normal and they are skipped automatically.

**OAuth redirect error**
→ Make sure `http://localhost:8081/callback` is listed exactly as a Redirect URL in your Battle.net developer portal client settings.

**Vault/Roster state lost after restart**
→ The Great Vault and Weekly Roster use browser `localStorage` — clearing browser data will reset them. Server restarts have no effect on this state.
