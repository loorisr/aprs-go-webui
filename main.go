package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	fap "github.com/hessu/go-aprs-fap"
)

//go:embed static/*
var staticFiles embed.FS

type Packet struct {
	Raw      string  `json:"raw"`
	Src      string  `json:"src"`
	Dst      string  `json:"dst,omitempty"`
	Type     string  `json:"type,omitempty"`
	Lat      float64 `json:"lat,omitempty"`
	Lon      float64 `json:"lon,omitempty"`
	HasPos   bool    `json:"hasPos"`
	Symbol   string  `json:"symbol,omitempty"`
	Comment  string  `json:"comment,omitempty"`
	Status   string  `json:"status,omitempty"`
	Message  string  `json:"message,omitempty"`
	Speed    float64 `json:"speed,omitempty"`
	Course   int     `json:"course,omitempty"`
	Altitude float64 `json:"altitude,omitempty"`
	Time     int64   `json:"time"`
}

type Config struct {
	HasPMTiles bool   `json:"hasPMTiles"`
	PMTilesURL string `json:"pmtilesUrl,omitempty"`
}

var (
	clientsMu    sync.RWMutex
	clients      = make(map[*websocket.Conn]bool)
	broadcast    = make(chan Packet, 1000)
	positions    = make(map[string]Packet)
	posMu        sync.RWMutex
	recentPkts   = make([]Packet, 0, 200)
	recentPktsMu sync.Mutex
)

var (
	pmtilesFile       string
	useRemotePmtiles  bool
	remotePmtilesURL  string
	csvFile           string
	csvWriter         *csv.Writer
	csvBuf      *bufio.Writer
	csvFd       *os.File
	libDir      = "lib"
	libFiles    = []struct{ url, name string }{
		{"https://unpkg.com/maplibre-gl@5/dist/maplibre-gl.js", "maplibre-gl.js"},
		{"https://unpkg.com/maplibre-gl@5/dist/maplibre-gl.js.map", "maplibre-gl.js.map"},
		{"https://unpkg.com/maplibre-gl@5/dist/maplibre-gl.css", "maplibre-gl.css"},
		{"https://unpkg.com/pmtiles@4/dist/pmtiles.js", "pmtiles.js"},
		{"https://unpkg.com/pmtiles@4/dist/pmtiles.js.map", "pmtiles.js.map"},
		{"https://unpkg.com/@protomaps/basemaps@5/dist/basemaps.js", "basemaps.js"},
		{"https://unpkg.com/@protomaps/basemaps@5/dist/basemaps.js.map", "basemaps.js.map"},
	}
)

func main() {
	listenAddr := flag.String("listen", ":8080", "HTTP listen address")
	aprsServer := flag.String("server", "rotate.aprs.net:14580", "APRS-IS server address")
	callsign := flag.String("callsign", "", "APRS-IS callsign (needed unless -kiss is used)")
	aprsFilter := flag.String("filter", "t/poimqstunw", "APRS-IS filter string (e.g. t/p for positions, r/lat/lon/dist for range)")
	kissAddr := flag.String("kiss", "", "KISS TNC address (e.g. localhost:8001 for Direwolf)")
	flag.StringVar(&pmtilesFile, "pmtiles", "", "Path to PMTiles file (optional)")
	forceOffline := flag.Bool("offline", false, "Force offline mode, skip remote PMTiles")
	flag.StringVar(&csvFile, "csv", "", "Path to CSV output file (optional)")
	flag.Parse()

	if *callsign == "" && *kissAddr == "" {
		log.Fatal("either -callsign or -kiss is required")
	}

	if pmtilesFile != "" && !*forceOffline {
		today := time.Now().UTC().Format("20060102")
		remoteURL := "https://build.protomaps.com/" + today + ".pmtiles"
		if checkOnline(remoteURL) {
			useRemotePmtiles = true
			remotePmtilesURL = remoteURL
			log.Printf("Internet detected, proxying remote PMTiles: %s", remoteURL)
		} else {
			useRemotePmtiles = false
			log.Printf("No internet, using local PMTiles: %s", pmtilesFile)
		}
	}

	if csvFile != "" {
		fd, err := os.Create(csvFile)
		if err != nil {
			log.Fatalf("Cannot create CSV file %s: %v", csvFile, err)
		}
		csvFd = fd
		csvBuf = bufio.NewWriter(fd)
		csvWriter = csv.NewWriter(csvBuf)
		csvWriter.Write([]string{
			"time", "raw", "src", "dst", "type", "lat", "lon",
			"symbol", "comment", "status", "message",
			"speed", "course", "altitude",
		})
		csvWriter.Flush()
		log.Printf("CSV output: %s", csvFile)
	}

	mux := http.NewServeMux()

	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	os.MkdirAll(libDir, 0755)
	downloadLibs()
	mux.Handle("/lib/", http.StripPrefix("/lib/", http.FileServer(http.Dir(libDir))))

	if pmtilesFile != "" {
		mux.HandleFunc("/map.pmtiles", servePMTiles)
	}

	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/ws", handleWebSocket)

	go runAPRSClient(*callsign, *aprsServer, *aprsFilter)
	if *kissAddr != "" {
		go runKISSClient(*kissAddr)
	}
	go broadcastPositions()

	log.Printf("APRS Go Map starting on %s", *listenAddr)
	log.Printf("APRS-IS server: %s", *aprsServer)
	log.Printf("APRS-IS filter: %s", *aprsFilter)
	if pmtilesFile != "" {
		if useRemotePmtiles {
			log.Printf("PMTiles mode: remote (%s)", remotePmtilesURL)
		} else {
			log.Printf("PMTiles mode: local (%s)", pmtilesFile)
		}
	} else {
		log.Printf("No PMTiles file specified, using OSM raster tiles")
	}

	log.Fatal(http.ListenAndServe(*listenAddr, mux))
}

func servePMTiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD")
	w.Header().Set("Access-Control-Allow-Headers", "Range")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Etag, Accept-Ranges")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if useRemotePmtiles {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, remotePmtilesURL, nil)
		if err != nil {
			http.Error(w, "proxy error", http.StatusInternalServerError)
			return
		}
		req.Header.Set("Range", r.Header.Get("Range"))
		req.Header.Set("If-Range", r.Header.Get("If-Range"))

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "gateway error", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, v := range resp.Header {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	http.ServeFile(w, r, pmtilesFile)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	cfg := Config{HasPMTiles: false}
	if pmtilesFile != "" {
		cfg.HasPMTiles = true
		cfg.PMTilesURL = "/map.pmtiles"
	}
	json.NewEncoder(w).Encode(cfg)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("WebSocket accept error: %v", err)
		return
	}

	clientsMu.Lock()
	clients[conn] = true
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, conn)
		clientsMu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	ctx := context.Background()

	posMu.RLock()
	for _, pkt := range positions {
		data, _ := json.Marshal(pkt)
		conn.Write(ctx, websocket.MessageText, data)
	}
	posMu.RUnlock()

	recentPktsMu.Lock()
	for _, pkt := range recentPkts {
		data, _ := json.Marshal(pkt)
		conn.Write(ctx, websocket.MessageText, data)
	}
	recentPktsMu.Unlock()

	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			break
		}
	}
}

func broadcastPositions() {
	ctx := context.Background()

	for pkt := range broadcast {
		if pkt.HasPos {
			posMu.Lock()
			positions[pkt.Src] = pkt
			posMu.Unlock()
		}

		recentPktsMu.Lock()
		recentPkts = append(recentPkts, pkt)
		if len(recentPkts) > 200 {
			recentPkts = recentPkts[len(recentPkts)-200:]
		}
		recentPktsMu.Unlock()

		data, err := json.Marshal(pkt)
		if err != nil {
			continue
		}

		clientsMu.RLock()
		for conn := range clients {
			conn.Write(ctx, websocket.MessageText, data)
		}
		clientsMu.RUnlock()
	}
}

func runAPRSClient(callsign, server, filter string) {
	for {
		c, err := fap.Dial(server, callsign, "-1", "aprs-go-map", "1.0", filter)
		if err != nil {
			log.Printf("APRS-IS connection error: %v (retrying in 10s)", err)
			time.Sleep(10 * time.Second)
			continue
		}

		log.Printf("Connected to APRS-IS at %s as %s", server, callsign)

		for {
			raw, err := c.ReadPacket(30 * time.Second)
			if err != nil {
				log.Printf("APRS read error: %v", err)
				break
			}

			p, err := fap.Parse(raw)
			if err != nil {
				continue
			}

			pkt := Packet{
				Raw:  raw,
				Src:  p.SrcCallsign,
				Dst:  p.DstCallsign,
				Type: string(p.Type),
				Time: time.Now().Unix(),
			}

			if p.Latitude != nil && p.Longitude != nil {
				pkt.HasPos = true
				pkt.Lat = *p.Latitude
				pkt.Lon = *p.Longitude
			}

			if p.Comment != "" {
				pkt.Comment = p.Comment
			}
			if p.SymbolTable != 0 {
				pkt.Symbol = string([]byte{p.SymbolTable, p.SymbolCode})
			}
			if p.Status != "" {
				pkt.Status = p.Status
			}
			if p.Message != nil {
				pkt.Message = p.Message.Text
			}
			if p.Speed != nil {
				pkt.Speed = *p.Speed
			}
			if p.Course != nil {
				pkt.Course = *p.Course
			}
			if p.Altitude != nil {
				pkt.Altitude = *p.Altitude
			}

			csvRow(pkt)

			select {
			case broadcast <- pkt:
			default:
			}
		}

		c.Close()
		log.Printf("APRS-IS connection lost, reconnecting in 10s...")
		time.Sleep(10 * time.Second)
	}
}

func csvRow(pkt Packet) {
	if csvWriter == nil {
		return
	}
	csvWriter.Write([]string{
		time.Unix(pkt.Time, 0).UTC().Format(time.RFC3339),
		pkt.Raw,
		pkt.Src,
		pkt.Dst,
		pkt.Type,
		fmtF(pkt.Lat, pkt.HasPos),
		fmtF(pkt.Lon, pkt.HasPos),
		pkt.Symbol,
		pkt.Comment,
		pkt.Status,
		pkt.Message,
		fmtF(pkt.Speed, pkt.Speed != 0),
		fmtI(pkt.Course, pkt.Course != 0),
		fmtF(pkt.Altitude, pkt.Altitude != 0),
	})
	csvWriter.Flush()
	csvBuf.Flush()
}

func fmtF(v float64, ok bool) string {
	if !ok {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 5, 64)
}

func fmtI(v int, ok bool) string {
	if !ok {
		return ""
	}
	return strconv.Itoa(v)
}

func downloadLibs() {
	timeout := 30 * time.Second
	for _, lf := range libFiles {
		go ensureLibFile(lf.name, lf.url, timeout)
	}
}

func checkOnline(url string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Head(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func ensureLibFile(name, url string, timeout time.Duration) {
	path := filepath.Join(libDir, name)
	if _, err := os.Stat(path); err == nil {
		return
	}

	log.Printf("Downloading %s ...", name)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("Download %s failed: %v (will retry next start)", name, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Download %s failed: HTTP %d (will retry next start)", name, resp.StatusCode)
		return
	}

	f, err := os.Create(path)
	if err != nil {
		log.Printf("Failed to write %s: %v", name, err)
		return
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		log.Printf("Failed to write %s: %v", name, err)
		os.Remove(path)
		return
	}

	log.Printf("Downloaded %s", name)
}

func runKISSClient(addr string) {
	for {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			log.Printf("KISS connection error: %v (retrying in 10s)", err)
			time.Sleep(10 * time.Second)
			continue
		}

		log.Printf("Connected to KISS TNC at %s", addr)
		readKISSFrames(conn)
		conn.Close()
		log.Printf("KISS connection lost, reconnecting in 10s...")
		time.Sleep(10 * time.Second)
	}
}

func readKISSFrames(conn net.Conn) {
	buf := make([]byte, 4096)
	var frame []byte
	inFrame := false
	escaped := false

	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}

		for i := 0; i < n; i++ {
			b := buf[i]
			switch {
			case b == 0xC0: // FEND
				if inFrame && len(frame) > 0 {
					processKISSFrame(frame)
				}
				inFrame = true
				frame = frame[:0]
				escaped = false
			case b == 0xDB: // FESC
				escaped = true
			case escaped:
				if b == 0xDC {
					frame = append(frame, 0xC0)
				} else if b == 0xDD {
					frame = append(frame, 0xDB)
				}
				escaped = false
			default:
				frame = append(frame, b)
			}
		}
	}
}

func processKISSFrame(data []byte) {
	if len(data) < 2 {
		return
	}

	cmd := data[0]
	if cmd&0x0F != 0x00 {
		return
	}

	ax25 := data[1:]
	aprs := extractAPRSFromAX25(ax25)
	if aprs == "" {
		return
	}

	p, err := fap.Parse(aprs)
	if err != nil {
		log.Printf("KISS parse error: %v", err)
		return
	}

	pkt := Packet{
		Raw:  aprs,
		Src:  p.SrcCallsign,
		Dst:  p.DstCallsign,
		Type: string(p.Type),
		Time: time.Now().Unix(),
	}

	if p.Latitude != nil && p.Longitude != nil {
		pkt.HasPos = true
		pkt.Lat = *p.Latitude
		pkt.Lon = *p.Longitude
	}
	if p.Comment != "" {
		pkt.Comment = p.Comment
	}
	if p.SymbolTable != 0 {
		pkt.Symbol = string([]byte{p.SymbolTable, p.SymbolCode})
	}
	if p.Status != "" {
		pkt.Status = p.Status
	}
	if p.Message != nil {
		pkt.Message = p.Message.Text
	}
	if p.Speed != nil {
		pkt.Speed = *p.Speed
	}
	if p.Course != nil {
		pkt.Course = *p.Course
	}
	if p.Altitude != nil {
		pkt.Altitude = *p.Altitude
	}

	csvRow(pkt)

	select {
	case broadcast <- pkt:
	default:
	}
}

func extractAPRSFromAX25(data []byte) string {
	if len(data) < 16 {
		return ""
	}

	pos := 0
	var addrs []string

	for {
		if pos+7 > len(data) {
			return ""
		}
		addr := decodeAX25(data[pos : pos+7])
		addrs = append(addrs, addr)
		last := data[pos+6]&0x01 == 0x01
		pos += 7
		if last {
			break
		}
	}

	if len(addrs) < 2 {
		return ""
	}

	if pos+2 > len(data) {
		return ""
	}
	pos += 2

	if pos >= len(data) {
		return ""
	}

	info := string(data[pos:])

	src := addrs[1]
	dst := addrs[0]
	header := src + ">" + dst
	if len(addrs) > 2 {
		for _, digi := range addrs[2:] {
			header += "," + digi
		}
	}

	return header + ":" + info
}

func decodeAX25(addr []byte) string {
	call := make([]byte, 6)
	for i := 0; i < 6; i++ {
		call[i] = addr[i] >> 1
	}
	callStr := string(call)
	callStr = strings.TrimRight(callStr, " ")
	ssid := (addr[6] >> 1) & 0x0F
	if ssid > 0 {
		callStr += "-" + strconv.Itoa(int(ssid))
	}
	return callStr
}
