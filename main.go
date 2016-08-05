package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	tuf "github.com/flynn/go-tuf/client"
	tufdata "github.com/flynn/go-tuf/data"
	"github.com/jackc/pgx"
)

func main() {
	var keys []*tufdata.Key
	if err := json.Unmarshal([]byte(os.Getenv("ROOT_KEYS")), &keys); err != nil {
		log.Fatal("missing or invalid ROOT_KEYS:", err)
	}
	opts := &tuf.HTTPRemoteOptions{
		UserAgent: "cli-redirect/v1",
	}
	remote, err := tuf.HTTPRemoteStore(os.Getenv("REPO_URL"), opts)
	if err != nil {
		log.Fatal("error initializing remote store:", err)
	}
	r := &redirector{
		RepoURL: os.Getenv("REPO_URL"),
		Client:  tuf.NewClient(tuf.MemoryLocalStore(), remote),
		refresh: make(chan struct{}, 1),
		notify:  make(chan struct{}, 1),
	}
	if err := r.Client.Init(keys, len(keys)); err != nil {
		log.Fatal("error initializing client:", err)
	}
	if _, err := r.Client.Update(); err != nil {
		log.Fatal("error running first update:", err)
	}
	targets, err := r.Client.Targets()
	if err != nil {
		log.Fatal("error getting targets:", err)
	}
	r.Targets.Store(targets)

	pgConf, err := pgx.ParseURI(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("error parsing DATABASE_URL:", err)
	}
	r.DB, err = pgx.NewConnPool(pgx.ConnPoolConfig{ConnConfig: pgConf})
	if err != nil {
		log.Fatal("error creating pgx pool:", err)
	}

	go r.pgListener()
	go r.pgNotifier()
	go r.tufLoader()

	log.Fatal(http.ListenAndServe(":"+os.Getenv("PORT"), r))
}

type redirector struct {
	RepoURL         string
	Client          *tuf.Client
	Targets         atomic.Value // map[string]tufdata.Files
	DB              *pgx.ConnPool
	refresh, notify chan struct{}
}

func (r *redirector) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/refresh" {
		r.maybeNotify()
		return
	}
	w.Header().Set("Cache-Control", "no-cache")

	if req.URL.Path == "/cli.ps1" {
		r.powershell(w, req)
		return
	}

	var plat string
	if p := strings.TrimPrefix(strings.TrimPrefix(req.URL.Path, "/cli"), "/"); p == "" {
		plat = guessPlat(req.UserAgent())
	} else if strings.Count(p, "-") == 1 {
		plat = p
	} else {
		http.Error(w, "unknown platform", 404)
		return
	}

	name := fmt.Sprintf("/flynn-%s.gz", plat)
	f, ok := r.targets()[name]
	if !ok {
		http.Error(w, "unknown target", 404)
		return
	}

	http.Redirect(w, req, r.url(name, f), 302)
}

func (r *redirector) targets() tufdata.Files {
	return r.Targets.Load().(tufdata.Files)
}

func (r *redirector) url(name string, file tufdata.FileMeta) string {
	return fmt.Sprintf("%s/targets/%x.%s", r.RepoURL, []byte(file.Hashes["sha512"]), name[1:])
}

func (r *redirector) pgListener() {
	var conn *pgx.Conn
	listen := func() (err error) {
		conn, err = r.DB.Acquire()
		if err != nil {
			return
		}
		if err = conn.Listen("refresh"); err != nil {
			return
		}
		for {
			_, err = conn.WaitForNotification(time.Second)
			if err == pgx.ErrNotificationTimeout {
				continue
			}
			if err != nil {
				return
			}
			r.maybeLoad()
		}
	}
	for {
		err := listen()
		log.Println("listen error:", err)
		if conn != nil {
			conn.Exec("UNLISTEN refresh")
			r.DB.Release(conn)
			conn = nil
		}
		time.Sleep(time.Second)
	}
}

func (r *redirector) pgNotify() {
	if _, err := r.DB.Exec("NOTIFY refresh"); err != nil {
		log.Print("error notifying", err)
	}
}

func (r *redirector) pgNotifier() {
	for range r.notify {
		r.pgNotify()
		// maximum of one notify per minute
		time.Sleep(time.Minute)
	}
}

func (r *redirector) tufLoader() {
	go func() {
		// reload every 15 minutes
		for range time.Tick(15 * time.Minute) {
			r.maybeLoad()
		}
	}()

	for range r.refresh {
		r.loadTUF()
		// maximum of one fetch per minute
		time.Sleep(time.Minute)
	}
}

func (r *redirector) maybeLoad() {
	select {
	case r.refresh <- struct{}{}:
	default:
	}
}

func (r *redirector) maybeNotify() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *redirector) loadTUF() {
	retryLater := func() { time.AfterFunc(time.Minute, r.maybeLoad) }
	if _, err := r.Client.Update(); err != nil {
		if tuf.IsLatestSnapshot(err) {
			return
		}
		log.Print("error running TUF update:", err)
		retryLater()
		return
	}
	targets, err := r.Client.Targets()
	if err != nil {
		log.Print("error getting targets:", err)
		retryLater()
		return
	}
	r.Targets.Store(targets)
}

func guessArch(ua string) string {
	if strings.Contains(ua, "i386") || strings.Contains(ua, "i686") {
		return "386"
	}
	return "amd64"
}

func isDarwin(ua string) bool {
	return strings.Contains(ua, "mac os x") || strings.Contains(ua, "darwin")
}

func guessOS(ua string) string {
	if isDarwin(ua) {
		return "darwin"
	}
	if strings.Contains(ua, "windows") {
		return "windows"
	}
	return "linux"
}

func guessPlat(ua string) string {
	ua = strings.ToLower(ua)
	return guessOS(ua) + "-" + guessArch(ua)
}
