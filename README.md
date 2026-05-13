# aprs-go-map

Real-time APRS client for Go. Connects to APRS-IS servers or local KISS TNC (Direwolf) and displays received packets on an interactive map with offline PMTiles support.

## Features

- **APRS-IS client** — connects to APRS internet servers with configurable filters
- **KISS TNC support** — connects to local Direwolf instances via TCP KISS interface
- **PMTiles maps** — offline vector maps via local `.pmtiles` files, or auto-proxy to daily Protomaps builds when online
- **Raster fallback** — uses OpenStreetMap tiles when no PMTiles file is available
- **Packet list** — full-width bottom drawer with sortable table (type, src, dst, lat, lon, symbol, comment, time), click to expand decoded details
- **CSV export** — save all received packets to CSV with full decoded fields
- **Offline-capable** — all JS/CSS libraries downloaded locally on first run

## Quick start

```bash
# Build
go build .

# Run with APRS-IS
./aprs-go-map -callsign N0CALL-10

# Run with local KISS TNC (Direwolf on port 8001)
./aprs-go-map -kiss localhost:8001

# Run with offline PMTiles map
./aprs-go-map -callsign N0CALL-10 -pmtiles france.pmtiles -offline

# Run with CSV logging
./aprs-go-map -callsign N0CALL-10 -csv packets.csv
```

Open http://localhost:8080 in a browser.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-listen` | `:8080` | HTTP listen address |
| `-server` | `rotate.aprs.net:14580` | APRS-IS server address |
| `-callsign` | (required unless `-kiss`) | APRS-IS callsign (e.g. `N0CALL-10`) |
| `-filter` | `t/poimqstunw` | APRS-IS filter (`t/p` for positions, `r/lat/lon/dist` for range) |
| `-kiss` | | KISS TNC TCP address (e.g. `localhost:8001` for Direwolf) |
| `-pmtiles` | | Path to local PMTiles file for offline maps |
| `-offline` | `false` | Force offline mode (skip remote PMTiles proxy) |
| `-csv` | | Path to CSV output file for packet logging |

## PMTiles maps

When `-pmtiles` is specified and internet is available, the server proxies tile requests to `https://build.protomaps.com/YYYYMMDD.pmtiles` (today's daily build). When offline, the local `.pmtiles` file is served directly. Use `-offline` to force local mode.

To download a regional extract for testing:

```bash
# Download pmtiles CLI
https://github.com/protomaps/go-pmtiles/releases

# Extract a region (e.g., Paris, max zoom 12)
pmtiles extract https://build.protomaps.com/YYYYMMDD.pmtiles france.pmtiles \
  --bbox=2.0,48.5,2.6,49.0 --maxzoom=12
```

## KISS TNC (Direwolf)

Connect to a Direwolf KISS TNC over TCP:

```bash
./aprs-go-map -kiss localhost:8001
```

Can be combined with APRS-IS:

```bash
./aprs-go-map -callsign N0CALL-10 -kiss localhost:8001
```