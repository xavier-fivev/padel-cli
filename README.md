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

`padel auto-book` is a strict personal automation for Indoor Padel Australia Alexandria. It computes the target date as the current date in `Australia/Sydney` plus 14 days, only proceeds for Monday through Thursday target dates, and only considers 90-minute slots starting from 18:30 through 20:00 inclusive.

It defaults to `dry_run: true`, so the first runs only log what would happen.

```bash
cp config.example.yaml config.yaml

# Fill venue.id after confirming the Playtomic tenant id:
padel clubs --near "Alexandria NSW" --json

# Login once; credentials are stored under ~/.config/padel/credentials.json
padel auth login --email you@example.com

# Local dry-run test outside the release window
padel auto-book --config config.yaml --ignore-release-window
```

Required production settings:

- `dry_run: false` only after dry-run output and audit logs look correct.
- `venue.id` must resolve to exactly `Indoor Padel Australia Alexandria`.
- `payment.method` defaults to `MERCHANT_WALLET`. Use another exact Playtomic method code only if you have independently confirmed that your account exposes it as a saved payment method.
- Optional Telegram notifications use `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` unless you change the env var names in config.
- Optional calendar conflict checks use `calendar.ical_url`; if any selected 90-minute slot overlaps a VEVENT, that slot is skipped. Daily and weekly RRULE events are expanded; unsupported recurrence forms fail closed.

Schedule it at the release time in the Sydney timezone:

```cron
TZ=Australia/Sydney
30 18 * * * /path/to/padel auto-book --config /path/to/config.yaml >> /path/to/padel-auto-book.log 2>&1
```

The command only retries inside the release window from 18:30 to 18:35, with conservative 15-30 second polling. It writes decision and attempt audit events into `~/.config/padel/bookings.db` in the `auto_book_audit` table, and confirmed bookings into the existing `bookings` table.

Safety behaviour:

- Refuses config that widens the approved venue, timezone, release timing, weekdays, start window, duration, or booking caps.
- Syncs Playtomic bookings before checking the per-day and per-week caps.
- Stops and notifies on missing/expired login that cannot be refreshed, venue mismatch, iCalendar errors, payment method mismatch, checkout challenge indicators, or an unexpected confirmation payload.
- Does not implement CAPTCHA, MFA, 3DS, or payment-challenge bypasses.

Checkout notes:

The existing repository books through Playtomic payment intents and has observed `MERCHANT_WALLET` as the wallet-style payment method. Playtomic Wallet balance handling and saved-card challenge flows are not explicitly exposed by the current reverse-engineered API code. The automation therefore requires the configured payment method to appear in the intent response and requires confirmation to return a booking id; otherwise it stops instead of guessing or continuing through an interactive checkout state.

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
