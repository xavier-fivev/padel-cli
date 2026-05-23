# padel CLI

CLI tool for checking Playtomic padel court availability and booking.

## Install / Build

```bash
go build -o padel
```

## Nix

```bash
# Build with Nix flakes
nix build
./result/bin/padel-cli --help
```

## Openclaw Plugin

This repo exports an `openclawPlugin` flake output for nix-openclaw. nix-openclaw
symlinks skills into `~/.openclaw/workspace/skills/<skill>` and adds the plugin
packages to `PATH`, so no `skills.load.extraDirs` is needed.

## Usage

```bash
# List clubs near a location
padel clubs --near "Madrid"

# Check availability for a club on a date
padel availability --club-id <id> --date 2025-01-05

# Search for available courts
padel search --location "Barcelona" --date 2025-01-05 --time 18:00-22:00

# JSON output
padel clubs --near "Madrid" --json
```

## Venue Management

Save venues with aliases for quick access:

```bash
# Add a venue
padel venues add --id "<playtomic-id>" --alias myclub --name "My Club" --indoor --timezone "Europe/Madrid"

# List saved venues
padel venues list

# Use alias in commands
padel availability --venue myclub --date 2025-01-05

# Search multiple venues
padel search --venues myclub,otherclub --date 2025-01-05 --time 09:00-11:00
```

## Booking History

```bash
# List upcoming bookings
padel bookings list

# List past bookings
padel bookings list --past

# Add a booking manually
padel bookings add --venue myclub --date 2025-01-04 --time 10:30 --court "Court 5" --price 42

# Sync from Playtomic account
padel bookings sync

# View stats
padel bookings stats
```

## Authentication

```bash
# Login to Playtomic
padel auth login --email you@example.com --password yourpass

# Check status
padel auth status

# Book a court (requires auth)
padel book --venue myclub --date 2025-01-05 --time 10:30 --duration 90
```

## Autonomous Booking

`padel auto-book` is a strict personal automation for Indoor Padel Australia Alexandria. It computes the target date as the current date in `Australia/Sydney` plus 14 days, runs only for one of two configured profiles, and only books in the 18:30–18:35 Sydney release window.

### Doctrine

The strategy is **pre-grab privately at release, you decide within 48h**:

1. The bot races at 18:30 Sydney to grab a great slot the moment Playtomic releases it for non-gold members (14 days out).
2. The booking is **always private** — the bot never publishes a match to Playtomic's open feed, because publishing forfeits the free-cancel window.
3. Every confirmed booking includes a cancel deadline in the audit log and Telegram message: free cancel is allowed up to 48h before play. The bot also refuses to book any slot less than 72h away, so there's always a ≥24h margin above the cancel deadline.
4. Within 48h before play you either (a) invite 3 players in the Playtomic app and keep the booking, or (b) cancel for a full wallet refund.

The publish endpoint is structurally forbidden — `api/forbidden_test.go` fails CI if any future change adds a method that hits Playtomic's matches-publish path.

### Profiles

| Profile | Days | Start window | Duration | Caps (shared) |
|---|---|---|---|---|
| `weekday` (default) | Mon–Thu | 18:30–20:00 | 90 min | 1 per day, 3 per week |
| `weekend` | Sat–Sun | 10:00–18:00 | 90 or 120 min (prefers 90) | 1 per day, 3 per week |

The validator refuses any config that widens beyond the profile bounds.

### Setup

```bash
cp config.example.yaml config.yaml

# Confirm the Playtomic tenant id:
padel clubs --near "Alexandria NSW" --json

# Login once; credentials are stored under ~/.config/padel/credentials.json
padel auth login --email you@example.com

# Local dry-run test outside the release window
padel auto-book --config config.yaml --ignore-release-window
```

`dry_run: true` is the default — the first runs only log what would happen. Flip to `false` only after both dry-run output and `auto_book_audit` rows look correct.

### Production schedule

Run both profiles at the release time in Sydney:

```cron
TZ=Australia/Sydney
30 18 * * * /path/to/padel auto-book --config /path/to/config.weekday.yaml >> /path/to/padel-auto-book.log 2>&1
30 18 * * * /path/to/padel auto-book --config /path/to/config.weekend.yaml >> /path/to/padel-auto-book.log 2>&1
```

Each command retries inside 18:30–18:35 with conservative 15–30 second polling. The weekday config no-ops on weekend target dates and vice versa, so running both is safe.

### Required settings

- `mode`: `weekday` or `weekend`. Each picks the right weekdays, window, and durations.
- `dry_run: false` only after dry-run output looks correct.
- `venue.id` must resolve to exactly `Indoor Padel Australia Alexandria`.
- `payment.method` defaults to `MERCHANT_WALLET`. Use another exact Playtomic method code only if you have independently confirmed that your account exposes it as a saved payment method.
- Optional Telegram notifications use `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` unless you change the env var names in config.
- Optional calendar conflict checks use `calendar.ical_url`; if any selected slot overlaps a VEVENT, that slot is skipped. Daily and weekly RRULE events are expanded; unsupported recurrence forms fail closed.

### Audit trail

Every decision is written to `~/.config/padel/bookings.db` in the `auto_book_audit` table; confirmed bookings land in the `bookings` table with `source: auto_booked`. The `booking_confirmed` audit event includes `cancel_deadline_local` and `cancel_deadline_utc` so you can sort by what needs your attention soonest.

### Safety behaviour

- Refuses config that widens venue, timezone, release timing, profile weekdays, profile start window, allowed durations, or booking caps.
- Refuses to book any slot less than 72h away (defense in depth above today+14).
- Syncs Playtomic bookings before checking the per-day and per-week caps.
- Stops and notifies on missing/expired login that cannot be refreshed, venue mismatch, iCalendar errors, payment method mismatch, checkout challenge indicators, or an unexpected confirmation payload.
- Does not implement CAPTCHA, MFA, 3DS, or payment-challenge bypasses.
- Does not publish matches; the publish endpoint is structurally forbidden.

### Why no SPLIT / open-match support

Playtomic's iOS app can create open matches that only charge each player their share, but the customer-match endpoints exposed to public bearer tokens only accept the `SINGLE_PAYER` shape — the full court is charged to the booking owner. Probes for `split_payment_parts`, `payment_plan: SPLIT`, multi-registration payloads, and direct `PATCH` on the match's `payment_type` all silently dropped the field or returned 500/403. The SPLIT mechanism appears to live in an internal endpoint not reachable with our token. Until that changes, the safest play is what's documented above: book privately, decide within 48h.

## Indoor/Outdoor Filtering

Default shows indoor courts only:

```bash
# Indoor only (default)
padel search --venues myclub --date 2025-01-05

# Outdoor only
padel search --venues myclub --date 2025-01-05 --outdoor

# All courts
padel search --venues myclub --date 2025-01-05 --all
```

## Output Formats

- Default: human-readable tables
- `--json`: structured JSON output
- `--compact`: single-line summaries (useful for chat bots)

## Configuration

Config stored in `~/.config/padel/`:

```
~/.config/padel/
├── config.json          # preferences
├── credentials.json     # auth tokens
├── venues.json          # saved venues
└── bookings.db          # SQLite booking history
```

Environment overrides:

- `PADEL_CONFIG_DIR`: override the config directory (defaults to `~/.config/padel`)
- `XDG_CONFIG_HOME`: used if set and `PADEL_CONFIG_DIR` is not set
- `PADEL_AUTH_FILE`: default for `padel auth login --auth-file`

Example config.json:

```json
{
  "default_location": "Madrid",
  "favourite_clubs": [
    {"id": "abc123", "alias": "myclub"}
  ],
  "preferred_times": ["18:00", "19:30"],
  "preferred_duration": 90
}
```

## API Notes

Uses Playtomic API endpoints reverse-engineered from:
- https://mattrighetti.com/2025/03/03/reverse-engineering-playtomic
- https://github.com/ypk46/playtomic-scheduler

## License

MIT
