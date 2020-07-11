package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"           // driver="postgres"
	_ "github.com/mattn/go-sqlite3" // driver="sqlite3"

	"github.com/brianolson/login/login"
)

func maybefail(err error, format string, args ...interface{}) {
	if err == nil {
		return
	}
	log.Printf(format, args...)
	os.Exit(1)
}

func maybeerr(w http.ResponseWriter, err error, code int, format string, args ...interface{}) bool {
	if err == nil {
		return false
	}
	msg := fmt.Sprintf(format, args...)
	if code >= 500 || true {
		log.Print(msg, "\t", err)
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	w.Write([]byte(msg))
	return true
}

func texterr(w http.ResponseWriter, code int, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if code >= 500 {
		log.Print(msg)
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	w.Write([]byte(msg))
}

type dbSource struct {
	dbfactory  func() (*sql.DB, error)
	edbfactory func(*sql.DB) electionAppDB
}

func (ds dbSource) getDbs(w http.ResponseWriter, r *http.Request) (edb electionAppDB, udb login.UserDB, cf func() error, fail bool) {
	db, err := ds.dbfactory()
	if maybeerr(w, err, 500, "could not open db, %v", err) {
		fail = true
		return
	}
	udb = login.NewSqlUserDB(db)
	edb = ds.edbfactory(db)
	cf = db.Close
	fail = false
	return
}

// handler of /election and /election/*{,.pdf,.png,_bubbles.json,/scan}
type StudioHandler struct {
	dbs *dbSource

	drawBackend string

	cache Cache

	scantemplate *template.Template
	home         *template.Template
	archiver     ImageArchiver

	authmods []*login.OauthCallbackHandler
}

var pdfPathRe *regexp.Regexp
var bubblesPathRe *regexp.Regexp
var pngPathRe *regexp.Regexp
var scanPathRe *regexp.Regexp
var docPathRe *regexp.Regexp

func init() {
	pdfPathRe = regexp.MustCompile(`^/election/(\d+)\.pdf$`)
	bubblesPathRe = regexp.MustCompile(`^/election/(\d+)_bubbles\.json$`)
	pngPathRe = regexp.MustCompile(`^/election/(\d+)\.png$`)
	scanPathRe = regexp.MustCompile(`^/election/(\d+)/scan$`)
	docPathRe = regexp.MustCompile(`^/election/(\d+)$`)
}

// implement http.Handler
func (sh *StudioHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	edb, udb, dbcf, fail := sh.dbs.getDbs(w, r)
	if fail {
		return
	}
	defer dbcf()
	user, _ := login.GetHttpUser(w, r, udb)
	path := r.URL.Path
	if path == "/election" {
		if r.Method == "POST" {
			sh.handleElectionDocPOST(w, r, edb, user, "", 0)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"nope"}`))
		return
	}
	// `^/election/(\d+)$`
	m := docPathRe.FindStringSubmatch(path)
	if m != nil {
		electionid, err := strconv.ParseInt(m[1], 10, 64)
		if maybeerr(w, err, 400, "bad item") {
			return
		}
		if r.Method == "GET" {
			sh.handleElectionDocGET(w, r, edb, user, electionid)
		} else if r.Method == "POST" {
			sh.handleElectionDocPOST(w, r, edb, user, m[1], electionid)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"nope"}`))
		}
		return
	}
	// `^/election/(\d+)\.pdf$`
	m = pdfPathRe.FindStringSubmatch(path)
	if m != nil {
		bothob, err := sh.getPdf(edb, m[1])
		if err != nil {
			he := err.(*httpError)
			maybeerr(w, he.err, he.code, he.msg)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(200)
		w.Write(bothob.Pdf)
		return
	}
	// `^/election/(\d+)_bubbles\.json$`
	m = bubblesPathRe.FindStringSubmatch(path)
	if m != nil {
		bothob, err := sh.getPdf(edb, m[1])
		if err != nil {
			he := err.(*httpError)
			maybeerr(w, he.err, he.code, he.msg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write(bothob.BubblesJson)
		return
	}
	// `^/election/(\d+)\.png$`
	m = pngPathRe.FindStringSubmatch(path)
	if m != nil {
		pngbytes, err := sh.getPng(edb, m[1])
		if err != nil {
			he := err.(*httpError)
			maybeerr(w, he.err, he.code, he.msg)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(200)
		w.Write(pngbytes)
		return
	}
	// `^/election/(\d+)/scan$`
	m = scanPathRe.FindStringSubmatch(path)
	if m != nil {
		// POST: receive image
		// GET: serve a page with image upload
		if r.Method == "POST" {
			sh.handleElectionScanPOST(w, r, edb, user, m[1])
			return
		}
		electionid, err := strconv.ParseInt(m[1], 10, 64)
		if maybeerr(w, err, 400, "bad item") {
			return
		}
		w.Header().Set("Content-Type", "text/html")
		ec := EditContext{}
		ec.set(electionid)
		sh.scantemplate.Execute(w, ec)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(200)
	sh.home.Execute(w, HomeContext{user, sh.authmods})
}

type HomeContext struct {
	User     *login.User
	AuthMods []*login.OauthCallbackHandler
}

func (sh *StudioHandler) handleElectionDocPOST(w http.ResponseWriter, r *http.Request, edb electionAppDB, user *login.User, itemname string, itemid int64) {
	if user == nil {
		texterr(w, http.StatusUnauthorized, "nope")
		return
	}
	mbr := http.MaxBytesReader(w, r.Body, 1000000)
	body, err := ioutil.ReadAll(mbr)
	if err == io.EOF {
		err = nil
	}
	if maybeerr(w, err, 400, "bad body") {
		return
	}
	var ob map[string]interface{}
	err = json.Unmarshal(body, &ob)
	if maybeerr(w, err, 400, "bad json") {
		return
	}
	if itemid != 0 {
		older, _ := edb.GetElection(itemid)
		if older != nil {
			if older.Owner != user.Guid {
				texterr(w, http.StatusUnauthorized, "nope")
				return
			}
		}
	}
	er := electionRecord{
		Id:    itemid,
		Owner: user.Guid,
		Data:  string(body),
	}
	newid, err := edb.PutElection(er)
	if maybeerr(w, err, 500, "db put fail") {
		return
	}
	sh.cache.Invalidate(itemname)
	sh.cache.Invalidate(itemname + ".png")
	er.Id = newid
	ec := EditContext{}
	ec.set(newid)
	out, err := json.Marshal(ec)
	if maybeerr(w, err, 500, "json ret prep") {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(out)
}

func (sh *StudioHandler) handleElectionDocGET(w http.ResponseWriter, r *http.Request, edb electionAppDB, user *login.User, itemid int64) {
	// Allow everything to be readable? TODO: flexible ACL?
	// if user == nil {
	// 	texterr(w, http.StatusUnauthorized, "nope")
	// 	return
	// }
	er, err := edb.GetElection(itemid)
	if maybeerr(w, err, 400, "no item") {
		return
	}
	// Allow everything to be readable? TODO: flexible ACL?
	// if user.Guid != er.Owner {
	// 	texterr(w, http.StatusForbidden, "nope")
	// 	return
	// }
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write([]byte(er.Data))
}

func (sh *StudioHandler) getPdf(edb electionAppDB, el string) (bothob *DrawBothOb, err error) {
	cr := sh.cache.Get(el)
	if cr != nil {
		bothob = cr.(*DrawBothOb)
	} else {
		electionid, err := strconv.ParseInt(el, 10, 64)
		if err != nil {
			return nil, &httpError{400, "bad item", err}
		}
		er, err := edb.GetElection(electionid)
		if err != nil {
			return nil, &httpError{400, "no item", err}
		}
		bothob, err = draw(sh.drawBackend, er.Data)
		if err != nil {
			return nil, &httpError{500, "draw fail", err}
		}
		sh.cache.Put(el, bothob, len(bothob.Pdf)+len(bothob.BubblesJson))
	}
	return
}

func (sh *StudioHandler) getPng(edb electionAppDB, el string) (pngbytes []byte, err error) {
	pngkey := el + ".png"
	cr := sh.cache.Get(pngkey)
	if cr != nil {
		pngbytes = cr.([]byte)
		return
	}
	var bothob *DrawBothOb
	bothob, err = sh.getPdf(edb, el)
	if err != nil {
		return nil, err
	}
	pngbytes, err = pdftopng(bothob.Pdf)
	if err != nil {
		return nil, &httpError{500, "png fail", err}
	}
	sh.cache.Put(pngkey, pngbytes, len(pngbytes))
	return
}

type httpError struct {
	code int
	msg  string
	err  error
}

func (he httpError) Error() string {
	return fmt.Sprintf("%d %s, %v", he.code, he.msg, he.err)
}

type editHandler struct {
	dbs *dbSource
	t   *template.Template
}

type EditContext struct {
	ElectionId    int64  `json:"itemid,omitepmty"`
	PDFURL        string `json:"pdf,omitepmty"`
	BubbleJSONURL string `json:"bubbles,omitepmty"`
	ScanFormURL   string `json:"scan,omitepmty"`
	PostURL       string `json:"post,omitempty"`
	EditURL       string `json:"edit,omitempty"`
	GETURL        string `json:"url,omitempty"`
	StaticRoot    string `json:"staticroot,omitempty"`
}

func (ec *EditContext) set(eid int64) {
	if eid == 0 {
		ec.PostURL = "/election"
	} else {
		ec.ElectionId = eid
		ec.PDFURL = fmt.Sprintf("/election/%d.pdf", eid)
		ec.BubbleJSONURL = fmt.Sprintf("/election/%d_bubbles.json", eid)
		ec.ScanFormURL = fmt.Sprintf("/election/%d/scan", eid)
		ec.PostURL = fmt.Sprintf("/election/%d", eid)
		ec.EditURL = fmt.Sprintf("/edit/%d", eid)
		ec.GETURL = fmt.Sprintf("/election/%d", eid)
	}
	ec.StaticRoot = "/static"
}

func (ec EditContext) Json() template.JS {
	x, err := json.Marshal(ec)
	if err != nil {
		return ""
	}
	//fmt.Printf("ec %s\n", string(x))
	return template.JS(string(x))
}

func (ec EditContext) JsonAttr() template.HTMLAttr {
	x, err := json.Marshal(ec)
	if err != nil {
		return template.HTMLAttr("")
	}
	//fmt.Printf("ec %s\n", string(x))
	return template.HTMLAttr(template.URLQueryEscaper(string(x)))
}

// http.Handler
// just fills out index.html template
func (edit *editHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	electionid := int64(0)
	if strings.HasPrefix(r.URL.Path, "/edit/") {
		xe, err := strconv.ParseInt(r.URL.Path[6:], 10, 64)
		if err == nil {
			electionid = xe
		}
	}
	w.Header().Set("Content-Type", "text/html")
	ec := EditContext{}
	ec.set(electionid)
	err := edit.t.Execute(w, ec)
	if err != nil {
		log.Printf("editHandler template error, %v", err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func addrGetPort(listenAddr string) int {
	x := strings.LastIndex(listenAddr, ":")
	if x < 0 {
		return 80
	}
	v, err := strconv.ParseInt(listenAddr[x+1:], 10, 32)
	if err != nil {
		return 80
	}
	return int(v)
}

func main() {
	var listenAddr string
	flag.StringVar(&listenAddr, "http", ":8180", "interface:port to listen on, default \":8180\"")
	var oauthConfigPath string
	flag.StringVar(&oauthConfigPath, "oauth-json", "", "json file with oauth configs")
	var sqlitePath string
	flag.StringVar(&sqlitePath, "sqlite", "", "path to sqlite3 db to keep local data in")
	var postgresConnectString string
	flag.StringVar(&postgresConnectString, "postgres", "", "connection string to postgres database")
	var drawBackend string
	flag.StringVar(&drawBackend, "draw-backend", "", "url to drawing backend")
	var imageArchiveDir string
	flag.StringVar(&imageArchiveDir, "im-archive-dir", "", "directory to archive uploaded scanned images to; will mkdir -p")
	var cookieKeyb64 string
	flag.StringVar(&cookieKeyb64, "cookie-key", "", "base64 of 16 bytes for encrypting cookies")
	var pidpath string
	flag.StringVar(&pidpath, "pid", "", "path to write process id to")
	flag.Parse()

	templates, err := template.ParseGlob("gotemplates/*.html")
	maybefail(err, "parse templates, %v", err)
	indextemplate := templates.Lookup("index.html")
	if indextemplate == nil {
		log.Print("no template index.html")
		os.Exit(1)
	}

	if cookieKeyb64 == "" {
		ck := login.GenerateCookieKey()
		log.Printf("-cookie-key %s", base64.StdEncoding.EncodeToString(ck))
	} else {
		ck, err := base64.StdEncoding.DecodeString(cookieKeyb64)
		maybefail(err, "-cookie-key, %v", err)
		err = login.SetCookieKey(ck)
		maybefail(err, "-cookie-key, %v", err)
	}

	var udb login.UserDB
	var db *sql.DB
	var edb electionAppDB
	var dbfactory func() (*sql.DB, error)
	var udbfactory func() (login.UserDB, error)
	var edbfactory func(db *sql.DB) electionAppDB

	if len(sqlitePath) > 0 {
		if len(postgresConnectString) > 0 {
			fmt.Fprintf(os.Stderr, "error, only one of -sqlite or -postgres should be set")
			os.Exit(1)
			return
		}
		var err error
		db, err = sql.Open("sqlite3", sqlitePath)
		maybefail(err, "error opening sqlite3 db %#v, %v", sqlitePath, err)
		udb = login.NewSqlUserDB(db)
		edbfactory = NewSqliteEDB
		dbfactory = func() (*sql.DB, error) {
			return sql.Open("sqlite3", sqlitePath)
		}
	} else if len(postgresConnectString) > 0 {
		var err error
		db, err = sql.Open("postgres", postgresConnectString)
		maybefail(err, "error opening postgres db %#v, %v", postgresConnectString, err)
		udb = login.NewSqlUserDB(db)
		edbfactory = NewPostgresEDB
		dbfactory = func() (*sql.DB, error) {
			return sql.Open("postgres", postgresConnectString)
		}
	} else {
		log.Print("warning, running with in-memory database that will disappear when shut down")
		var err error
		db, err = sql.Open("sqlite3", ":memory:")
		maybefail(err, "error opening sqlite3 memory db, %v", err)
		udb = login.NewSqlUserDB(db)
		edbfactory = NewSqliteEDB
		dbfactory = func() (*sql.DB, error) {
			return sql.Open("sqlite3", ":memory:")
		}
	}
	udbfactory = func() (login.UserDB, error) {
		xdb, err := dbfactory()
		if err != nil {
			return nil, err
		}
		return login.NewSqlUserDB(xdb), nil
	}
	defer db.Close()
	edb = edbfactory(db)
	err = edb.Setup()
	maybefail(err, "edb setup, %v", err)
	err = udb.Setup()
	maybefail(err, "udb setup, %v", err)
	inviteToken := randomInviteToken(2)
	edb.MakeInviteToken(inviteToken, time.Now().Add(30*time.Minute))
	log.Printf("http://localhost:%d/signup/%s", addrGetPort(listenAddr), inviteToken)
	ctx, cf := context.WithCancel(context.Background())
	defer cf()
	go gcThread(ctx, edb, 57*time.Minute)

	source := dbSource{dbfactory, edbfactory}

	var archiver ImageArchiver
	if imageArchiveDir != "" {
		archiver, err = NewFileImageArchiver(imageArchiveDir)
		maybefail(err, "image archive dir, %v", err)
	}
	sh := StudioHandler{
		dbs:          &source,
		drawBackend:  drawBackend,
		scantemplate: templates.Lookup("scanform.html"),
		home:         templates.Lookup("home.html"),
		archiver:     archiver,
	}
	edith := editHandler{&source, indextemplate}
	ih := inviteHandler{
		dbs:        &source,
		signupPage: templates.Lookup("signup.html"),
	}

	mith := makeInviteTokenHandler{
		edb, udb, templates.Lookup("invitetoken.html"),
	}

	mux := http.NewServeMux()
	mux.Handle("/election", &sh)
	mux.Handle("/election/", &sh)
	mux.Handle("/edit", &edith)
	mux.Handle("/edit/", &edith)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	var authmods []*login.OauthCallbackHandler
	if len(oauthConfigPath) > 0 {
		fin, err := os.Open(oauthConfigPath)
		maybefail(err, "%s: could not open, %v", oauthConfigPath, err)
		oc, err := login.ParseConfigJSON(fin)
		maybefail(err, "%s: bad parse, %v", oauthConfigPath, err)
		authmods, err = login.BuildOauthMods(oc, udbfactory, "/", "/")
		maybefail(err, "%s: oauth problems, %v", oauthConfigPath, err)
		for _, am := range authmods {
			mux.Handle(am.HandlerUrl(), am)
		}
	}
	ih.authmods = authmods
	sh.authmods = authmods
	mux.Handle("/signup/", &ih)
	log.Printf("initialized %d oauth mods", len(authmods))
	mux.HandleFunc("/logout", login.LogoutHandler)
	mux.Handle("/makeinvite", &mith)
	mux.Handle("/", &sh)
	server := http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}
	if pidpath != "" {
		pidf, err := os.Create(pidpath)
		if err != nil {
			log.Printf("could not create pidfile, %v", err)
			// meh, keep going
		} else {
			fmt.Fprintf(pidf, "%d", os.Getpid())
			pidf.Close()
		}
	}
	log.Print("serving ", listenAddr)
	log.Fatal(server.ListenAndServe())
}
