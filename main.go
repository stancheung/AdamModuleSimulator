package main

import (
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"strconv"
)

//go:embed templates assets
var files embed.FS

var tmpl *template.Template

type point struct {
	Idx   int
	Label string
	On    bool
}

type statusView struct {
	BtnLabel string
	Kind     string
	Text     string
}

func main() {
	addr := flag.String("addr", ":5002", "HTTP listen address")
	modIP := flag.String("ip", "", "IP advertised in ADAM discovery (default: auto-detect)")
	password := flag.String("password", "00000000", "module password for the ADAM utility")
	modName := flag.String("name", "6050", "module name advertised (6050 = ADAM-6050 DI/DO)")
	flag.Parse()

	// Windows maps .js/.css through the registry, often to text/plain, which
	// browsers refuse for scripts and stylesheets.
	mime.AddExtensionType(".js", "text/javascript")
	mime.AddExtensionType(".css", "text/css")
	mime.AddExtensionType(".woff2", "font/woff2")

	var err error
	tmpl, err = template.ParseFS(files, "templates/*.html")
	if err != nil {
		log.Fatalf("template error: %v", err)
	}

	adam := NewAdamServer()

	ip := *modIP
	if ip == "" {
		ip = detectLocalIP()
	}
	identity := &moduleIdentity{
		Name:     *modName,
		Desc:     "AdamModuleSimulator sim",
		DevID:    [2]byte{0x60, 0x50}, // ADAM-6050
		FwVer:    "4.02",              // < 5.x: utility skips flexible-modbus XML path
		MAC:      detectMAC(ip),
		IP:       ip,
		Subnet:   "255.255.255.0",
		Gateway:  gatewayFor(ip),
		Password: *password,
		Port:     502,
	}
	adam.identity = identity
	adam.StartListening()
	adam.startDiscovery(identity)
	log.Printf("ADAM discovery: advertising ADAM-%s at %s (pw %s)", identity.Name, identity.IP, identity.Password)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", adam.handleIndex)
	mux.HandleFunc("GET /fragments/coils", adam.handleCoils)
	mux.HandleFunc("GET /fragments/status", adam.handleStatus)
	mux.HandleFunc("POST /coils/{idx}/{value}", adam.handleForce)
	mux.HandleFunc("POST /listen/toggle", adam.handleToggle)

	assets, err := fs.Sub(files, "assets")
	if err != nil {
		log.Fatalf("assets error: %v", err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))

	log.Printf("AdamModuleSimulator web panel on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (adam *AdamServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "index.html", statusOf(adam.Status())); err != nil {
		log.Printf("index render: %v", err)
	}
}

func (adam *AdamServer) handleCoils(w http.ResponseWriter, r *http.Request) {
	coils := adam.PollCoils()

	diCount := 12
	if r.URL.Query().Get("model") == "6060" {
		diCount = 6
	}

	points := make([]point, 0, diCount+6)
	for idx := 0; idx < diCount; idx++ {
		points = append(points, point{idx, fmt.Sprintf("DI%d", idx), coils[idx] == 1})
	}
	for idx := 12; idx < 18; idx++ {
		points = append(points, point{idx, fmt.Sprintf("DO%d", idx-12), coils[idx] == 1})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "coils.html", points); err != nil {
		log.Printf("coils render: %v", err)
	}
}

func (adam *AdamServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "status.html", statusOf(adam.Status())); err != nil {
		log.Printf("status render: %v", err)
	}
}

func (adam *AdamServer) handleToggle(w http.ResponseWriter, r *http.Request) {
	if adam.Status().Listening {
		adam.StopListening()
	} else {
		adam.StartListening()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "listenbtn.html", statusOf(adam.Status())); err != nil {
		log.Printf("listen button render: %v", err)
	}
}

func (adam *AdamServer) handleForce(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 || idx >= 18 {
		http.Error(w, "bad coil index", http.StatusBadRequest)
		return
	}
	value := 0
	if r.PathValue("value") == "on" {
		value = 1
	}
	adam.ForceCoil(idx, value)
	w.WriteHeader(http.StatusNoContent)
}

func statusOf(s ServerStatus) statusView {
	switch {
	case s.Listening:
		return statusView{"Stop", "ok", "Modbus listening on " + s.Address}
	case s.Error != "":
		return statusView{"Start", "bad", "Modbus down: " + s.Error}
	default:
		return statusView{"Start", "warn", "Modbus listener stopped"}
	}
}
