package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"
)

var (
	hostPort = flag.String("hostport", "localhost:8080", "server host and port")
	repoName = flag.String("repo", "", "repo name")
	logPath  = flag.String("log", "", "Log file path, default is output")
	secret   = flag.String("secret", "", "Github notification secret")
	binary   = flag.String("binary", "default-name", "Builded binary name")
)

func main() {
	flag.Parse()

	if *logPath != "" {
		logOut, err := os.Open(*logPath)

		if err != nil {
			log.Fatal(err)
		}

		log.SetOutput(logOut)
	}

	if *repoName == "" {
		log.Fatal("Specify repo name using flag -repo=")
	}

	if *secret == "" {
		log.Fatal("Specify secret using flag -secret=")
	}

	r := httprouter.New()

	p := NewProxy(r, *repoName, *binary)
	err := p.firstBuild()

	if err != nil {
		log.Fatalln(err)
	}

	p.router.POST("/_github_push", httprouter.Handle(func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Printf("Request to /_github_push read body: %s", err)
			return
		}

		h := hmac.New(sha1.New, []byte(*secret))
		h.Write(body)
		sign := fmt.Sprintf("sha1=%s", hex.EncodeToString(h.Sum(nil)))

		if !hmac.Equal([]byte(r.Header.Get("X-Hub-Signature")), []byte(sign)) {
			// TODO(romanyx): ban it then
			log.Printf("Wrong signature from %s", r.RemoteAddr)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		pushEvnt := struct {
			Ref  string `json:"ref"`
			Head string `json:"after"`
		}{}

		if err := json.Unmarshal(body, &pushEvnt); err != nil {
			log.Printf("Request to /_github_push unmarshal: %s", err)
			return
		}

		if pushEvnt.Ref == "refs/heads/master" {
			fmt.Fprintf(w, "Thanks, updating to %s now", pushEvnt.Head)
			go p.changeSide(pushEvnt.Head)
			return
		}

		fmt.Fprintf(w, "Unnecessary inform, head %s", p.last)
	}))

	p.router.GET("/_status", httprouter.Handle(func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		fmt.Fprintf(w, "side=%d\nhead=%s\ndir=%s\nport=808%d", p.side, p.last, p.dir, p.side)
	}))

	p.router.GET("/", httprouter.Handle(func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		p.proxy.ServeHTTP(w, r)
	}))

	go http.ListenAndServe(*hostPort, p)

	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	log.Println(<-ch)

	err = p.clearPrevious()

	if err != nil {
		log.Fatalln(err)
	}
}

// Proxy is a struct to manage a traffic flow
type Proxy struct {
	proxy  *httputil.ReverseProxy
	router *httprouter.Router

	repo, binn string

	mu        sync.Mutex
	last, dir string
	side      int
	cmd       *exec.Cmd
}

// NewProxy returns initialized proxy
func NewProxy(r *httprouter.Router, repo, binn string) *Proxy {
	return &Proxy{side: 2, router: r, repo: repo, binn: binn}
}

// ServeHTTTP handler
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.router.ServeHTTP(w, r)
}

func (p *Proxy) clearPrevious() error {
	if p.dir != "" {
		err := os.RemoveAll(p.dir)

		if err != nil {
			return errors.Wrap(err, "removing previous directory")
		}
	}

	return nil
}

func (p *Proxy) changeSide(head string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	nSide := 1
	if p.side == 1 {
		nSide = 2
	}

	dir := filepath.Join(os.TempDir(), p.binn, strconv.Itoa(nSide))

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Println(errors.Wrap(err, "temp dir creation"))
			return
		}
	} else {
		err = os.RemoveAll(dir)

		if err != nil {
			log.Println(errors.Wrap(err, "temp dir remove previous"))
			return
		}
	}

	cloneCmd := exec.Command("git", "clone", fmt.Sprintf("https://github.com/%v", p.repo), ".")
	cloneCmd.Stdout = os.Stdout
	cloneCmd.Stderr = os.Stdout
	cloneCmd.Dir = dir
	if err := cloneCmd.Run(); err != nil {
		log.Println(errors.Wrap(err, "git clone"))
		return
	}

	fetchCmd := exec.Command("git", "fetch")
	fetchCmd.Stdout = os.Stdout
	fetchCmd.Stderr = os.Stdout
	fetchCmd.Dir = dir
	if err := fetchCmd.Run(); err != nil {
		log.Println(errors.Wrap(err, "git fetch"))
		return
	}

	resetCmd := exec.Command("git", "reset", "--hard", head)
	resetCmd.Stdout = os.Stdout
	resetCmd.Stderr = os.Stdout
	resetCmd.Dir = dir
	if err := resetCmd.Run(); err != nil {
		log.Println(errors.Wrap(err, "git reset"))
		return
	}

	cleanCmd := exec.Command("git", "clean", "-f", "-d", "-x")
	cleanCmd.Stdout = os.Stdout
	cleanCmd.Stderr = os.Stdout
	cleanCmd.Dir = dir
	if err := cleanCmd.Run(); err != nil {
		log.Println(errors.Wrap(err, "git clean"))
		return
	}

	installCmd := exec.Command("go", "build", "-o", p.binn)
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stdout
	installCmd.Dir = dir
	if err := installCmd.Run(); err != nil {
		log.Println(errors.Wrap(err, "go build -o"))
		return
	}

	lCmd := p.cmd

	runCmd := exec.Command(fmt.Sprintf("./%s", p.binn), "-hostport=localhost:808"+strconv.Itoa(nSide))
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stdout
	runCmd.Dir = dir

	go runCmd.Run()
	p.cmd = runCmd

	u, err := url.Parse(fmt.Sprintf("http://localhost:808%v/", strconv.Itoa(nSide)))

	p.proxy = httputil.NewSingleHostReverseProxy(u)

	if err != nil {
		log.Println(errors.Wrap(err, "url parse for proxying"))
		return
	}

	if lCmd != nil {
		if err = lCmd.Process.Kill(); err != nil {
			log.Println(errors.Wrap(err, "kill previous command"))
			return
		}
	}

	err = p.clearPrevious()

	if err != nil {
		log.Println(errors.Wrap(err, "remove previous directory"))
		return
	}

	p.side = nSide
	p.dir = dir
	p.last = head

	log.Printf("Project was rebuilded head now is %s", p.last)
}

func (p *Proxy) firstBuild() error {
	current, err := getCurrent()
	if err != nil {
		return errors.Wrap(err, "get current")
	}

	ok := p.last == current
	p.last = current

	if ok {
		return nil
	}

	p.changeSide(current)

	return nil
}

func getCurrent() (hash string, err error) {
	resp, err := http.Get(fmt.Sprintf("https://api.github.com/repos/%v/commits/master", *repoName))

	if err != nil {
		return "", errors.Wrap(err, "get request")
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get request %v", resp.Status)
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return "", errors.Wrap(err, "read body")
	}

	sha := struct {
		Sha string `json:"sha"`
	}{}

	err = json.Unmarshal(body, &sha)

	if err != nil {
		return "", errors.Wrap(err, "unmarshal json")
	}

	return sha.Sha, nil
}
