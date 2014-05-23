package main

import (
	"bufio"
	"bytes"
	"code.google.com/p/go.exp/fsnotify"
	"fmt"
	"gnd.la/admin"
	"gnd.la/app"
	"gnd.la/config"
	"gnd.la/internal/runtimeutil"
	"gnd.la/log"
	"gnd.la/util/generic"
	"go/build"
	"html/template"
	"io"
	"math/rand"
	"net"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	devConfigName = "dev.conf"
)

var (
	sourceExtensions = []string{
		".go",
		".h",
		".c",
		".s",
		".cpp",
		".cxx",
	}
	noColorRegexp = regexp.MustCompile("\x1b\\[\\d+;\\d+m(.*?)\x1b\\[00m")
	panicRe       = regexp.MustCompile("\npanic: (.+)")
)

func uncolor(s string) string {
	return noColorRegexp.ReplaceAllString(s, "$1")
}

func isSource(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	for _, v := range sourceExtensions {
		if ext == v {
			return true
		}
	}
	return false
}

func formatTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return strconv.FormatInt(t.Unix(), 10)
}

func exitStatus(p *os.ProcessState) int {
	ws := p.Sys().(syscall.WaitStatus)
	return ws.ExitStatus()
}

func cmdString(cmd *exec.Cmd) string {
	return strings.Join(cmd.Args, " ")
}

func supportsRace() bool {
	return runtime.GOARCH == "amd64" && (runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows")
}

func randomFreePort() int {
	for {
		mp := rand.Intn(65000)
		if mp < 10000 {
			continue
		}
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", mp))
		if err == nil {
			listener.Close()
			return mp
		}
	}
	panic("unreachable")
}

type BuildError struct {
	Package  string
	Filename string
	Line     int
	Error    string
}

func (b *BuildError) Location() string {
	return fmt.Sprintf("%s, line %d", b.Filename, b.Line)
}

func (b *BuildError) Code() template.HTML {
	s, err := runtimeutil.FormatSourceHTML(b.Filename, b.Line, 5, true, true)
	if err != nil {
		log.Errorf("Error formatting code from %s: %s", b.Filename, err)
	}
	return s
}

func NewProject(dir string, config string) *Project {
	p := &Project{
		dir:        dir,
		configPath: config,
	}
	a := app.New()
	a.Logger = nil
	a.SetTemplatesLoader(devAssets)
	a.Handle("/_gondola_dev_server_status", p.StatusHandler)
	a.Handle("/", p.Handler)
	a.Port = 8888
	p.App = a
	return p
}

type Project struct {
	sync.Mutex
	App        *app.App
	dir        string
	configPath string
	tags       string
	race       bool
	noDebug    bool
	noCache    bool
	profile    bool
	port       int
	proxy      *httputil.ReverseProxy
	buildCmd   *exec.Cmd
	errors     []*BuildError
	cmd        *exec.Cmd
	watcher    *fsnotify.Watcher
	built      time.Time
	started    time.Time
	// runtime info
	out      bytes.Buffer
	runError error
	exitCode int
}

func (p *Project) Listen() {
	os.Setenv("GONDOLA_IS_DEV_SERVER", "1")
	p.App.MustListenAndServe()
}

func (p *Project) Name() string {
	return filepath.Base(p.dir)
}

func (p *Project) buildTags() []string {
	tags := p.tags
	if p.profile {
		if tags == "" {
			tags = "profile"
		} else {
			tags += " profile"
		}
	}
	if tags != "" {
		return []string{"-tags", tags}
	}
	return nil
}

func (p *Project) importPackage(imported map[string]bool, pkgs *[]*build.Package, path string) error {
	if imported[path] {
		return nil
	}
	pkg, err := build.Import(path, p.dir, 0)
	if err != nil {
		return err
	}
	imported[path] = true
	*pkgs = append(*pkgs, pkg)
	for _, imp := range pkg.Imports {
		if imp == "C" {
			continue
		}
		if err := p.importPackage(imported, pkgs, imp); err != nil {
			return err
		}
	}
	return nil
}

// Packages returns the packages imported by the Project, either
// directly or transitively.
func (p *Project) Packages() ([]*build.Package, error) {
	var pkgs []*build.Package
	imported := make(map[string]bool)
	err := p.importPackage(imported, &pkgs, ".")
	return pkgs, err
}

func (p *Project) StopMonitoring() {
	if p.watcher != nil {
		p.watcher.Close()
		p.watcher = nil
	}
}

func (p *Project) StartMonitoring() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	var toWatch []string
	switch runtime.GOOS {
	case "darwin":
		// Watch GOROOT, GOPATH and the project dir. Any modification
		// to those dirs is likely to require a rebuild. The reason we
		// don't watch each pkg dir is because watch events are recursive
		// in OS X and watching all dirs will cause the process to open too many files.
		toWatch = []string{build.Default.GOROOT, build.Default.GOPATH, p.dir}
	default:
		pkgs, err := p.Packages()
		if err != nil {
			return err
		}
		toWatch = generic.Map(pkgs, func(pkg *build.Package) string { return pkg.Dir }).([]string)
	}
	for _, v := range toWatch {
		if err := watcher.Watch(v); err != nil {
			return err
		}
	}
	watcher.Watch(p.configPath)
	p.watcher = watcher
	go func() {
		var t *time.Timer
	finished:
		for {
			select {
			case ev := <-watcher.Event:
				if ev == nil {
					// Closed
					break finished
				}
				if ev.IsAttrib() {
					break
				}
				if ev.Name == p.configPath {
					if ev.IsDelete() {
						// It seems the Watcher stops watching a file
						// if it receives a DELETE event for it. For some
						// reason, some editors generate a DELETE event
						// for a file when saving it, so we must watch the
						// file again. Since fsnotify is in exp/ and its
						// API might change, remove the watch first, just
						// in case.
						watcher.RemoveWatch(ev.Name)
						watcher.Watch(ev.Name)
					} else {
						log.Infof("Config file %s changed, restarting...", p.configPath)
						if err := p.Stop(); err != nil {
							log.Errorf("Error stopping %s: %s", p.Name(), err)
							break
						}
						if err := p.Start(); err != nil {
							log.Panicf("Error starting %s: %s", p.Name(), err)
						}
					}
					break
				}
				if isSource(ev.Name) {
					if t != nil {
						t.Stop()
					}
					t = time.AfterFunc(50*time.Millisecond, func() {
						p.Build()
					})
				}
			case err := <-watcher.Error:
				if err == nil {
					// Closed
					break finished
				}
				log.Errorf("Error watching: %s", err)
			}
		}
	}()
	return nil
}

func (p *Project) ProjectCmd() *exec.Cmd {
	name := p.Name()
	if runtime.GOOS != "windows" {
		name = "./" + name
	}
	args := []string{"-config", p.configPath, fmt.Sprintf("-port=%d", p.port)}
	if p.noDebug {
		args = append(args, "-app-debug=false", "-template-debug=false", "-log-debug=false")
	} else {
		if p.profile {
			args = append(args, "-app-debug=false", "-template-debug=false", "-log-debug")
		} else {
			args = append(args, "-app-debug", "-template-debug", "-log-debug")
		}
	}
	if p.noCache {
		args = append(args, "-cache=dummy://")
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, &p.out)
	cmd.Stderr = io.MultiWriter(os.Stderr, &p.out)
	cmd.Dir = p.dir
	cmd.Env = append(cmd.Env, os.Environ()...)
	cmd.Env = append(cmd.Env, "GONDOLA_DEV_SERVER=1")
	cmd.Env = append(cmd.Env, "GONDOLA_FORCE_TTY=1")
	if p.profile {
		cmd.Env = append(cmd.Env, "GONDOLA_NO_CACHE_LAYER=1")
	}
	return cmd
}

func (p *Project) Start() error {
	p.Lock()
	defer p.Unlock()
	return p.startLocked()
}

func (p *Project) startLocked() error {
	p.port = randomFreePort()
	cmd := p.ProjectCmd()
	log.Infof("Starting %s (%s)", p.Name(), cmdString(cmd))
	p.cmd = cmd
	p.out.Reset()
	p.runError = nil
	p.exitCode = 0
	err := cmd.Start()
	go func() {
		werr := cmd.Wait()
		if cmd == p.cmd {
			// Othewise the process was intentionally killed
			if s := cmd.ProcessState; s != nil {
				exitCode := exitStatus(s)
				p.Lock()
				defer p.Unlock()
				p.runError = werr
				p.exitCode = exitCode
				log.Warningf("%s exited with code %d", p.Name(), exitCode)
			}
		}
	}()
	time.AfterFunc(100*time.Millisecond, p.projectStarted)
	return err
}

func (p *Project) projectStarted() {
	p.Lock()
	defer p.Unlock()
	u, err := url.Parse(fmt.Sprintf("http://localhost:%d", p.port))
	if err != nil {
		panic(err)
	}
	p.proxy = httputil.NewSingleHostReverseProxy(u)
	p.started = time.Now().UTC()
}

func (p *Project) Stop() error {
	p.Lock()
	defer p.Unlock()
	p.proxy = nil
	p.started = time.Time{}
	var err error
	cmd := p.cmd
	if cmd != nil {
		proc := cmd.Process
		if proc != nil {
			err = proc.Kill()
		}
		cmd.Wait()
		p.cmd = nil
	}
	if err != nil && strings.Contains(err.Error(), "already finished") {
		err = nil
	}
	return err
}

func (p *Project) GoCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("go", args...)
	cmd.Dir = p.dir
	return cmd
}

func (p *Project) CompilerCmd() *exec.Cmd {
	// -e reports all the errors
	args := []string{"build", "-gcflags", "-e"}
	if p.race && supportsRace() {
		args = append(args, "-race")
	}
	args = append(args, p.buildTags()...)
	lib := filepath.Join(p.dir, "lib")
	if st, err := os.Stat(lib); err == nil && st.IsDir() {
		// If there's a lib directory, add it to rpath
		args = append(args, []string{"-ldflags", "-r lib"}...)
	}
	return p.GoCmd(args...)
}

// Build builds the project. If the project was already building, the build
// is restarted.
func (p *Project) Build() {
	cmd := p.CompilerCmd()
	var restarted bool
	p.Lock()
	if p.buildCmd != nil {
		proc := p.buildCmd.Process
		if proc != nil {
			proc.Signal(os.Interrupt)
			restarted = true
		}
	}
	p.buildCmd = cmd
	p.StopMonitoring()
	p.Unlock()
	if err := p.Stop(); err != nil {
		log.Panic(err)
	}
	p.errors = nil
	if !restarted {
		log.Infof("Building %s (%s)", p.Name(), cmdString(cmd))
	}
	var buf bytes.Buffer
	cmd.Stderr = &buf
	err := p.buildCmd.Run()
	p.Lock()
	defer p.Unlock()
	if p.buildCmd != cmd {
		// Canceled by another build
		return
	}
	p.buildCmd = nil
	p.built = time.Now().UTC()
	os.Stderr.Write(buf.Bytes())
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			log.Panic(err)
		}
		if es := exitStatus(exitErr.ProcessState); es != 1 && es != 2 {
			// gc returns 1 when it can't find a package, 2 when there are compilation errors
			log.Panic(err)
		}
		r := bufio.NewReader(bytes.NewReader(buf.Bytes()))
		var pkg string
		for {
			eline, err := r.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				log.Panic(err)
			}
			if strings.HasPrefix(eline, "#") {
				pkg = strings.TrimSpace(eline[1:])
			} else if strings.HasPrefix(eline, "\t") {
				// Info related to the previous error. Let it
				// crash if we don't have a previous error, just
				// in case there are any circumstances where a line
				// starting with \t means something else in the future.
				// This way the problem will be easier to catch.
				be := p.errors[len(p.errors)-1]
				be.Error += fmt.Sprintf(" (%s)", strings.TrimSpace(eline))
			} else {
				parts := strings.SplitN(eline, ":", 3)
				filename := filepath.Clean(filepath.Join(p.dir, parts[0]))
				line, err := strconv.Atoi(parts[1])
				if err != nil {
					log.Panic(err)
				}
				be := &BuildError{
					Package:  pkg,
					Filename: filename,
					Line:     line,
					Error:    strings.TrimSpace(parts[2]),
				}
				p.errors = append(p.errors, be)
			}
		}
	}
	if c := len(p.errors); c == 0 {
		// TODO: Report error when starting project via web
		if err := p.startLocked(); err != nil {
			log.Panic(err)
		}
	} else {
		log.Errorf("%d errors building %s", c, p.Name())
	}
	if err := p.StartMonitoring(); err != nil {
		log.Errorf("Error monitoring files for project %s: %s. Development server must be manually restarted.", p.Name(), err)
	}
	// Build dependencies, to speed up future builds
	go func() {
		args := []string{"test", "-i"}
		args = append(args, p.buildTags()...)
		if p.race && supportsRace() {
			args = append(args, "-race")
		}
		p.GoCmd(args...).Run()
	}()
}

func (p *Project) Handler(ctx *app.Context) {
	if len(p.errors) > 0 {
		data := map[string]interface{}{
			"Project": p,
			"Errors":  p.errors,
			"Count":   len(p.errors),
			"Built":   formatTime(p.built),
			"Started": formatTime(p.started),
		}
		ctx.MustExecute("errors.html", data)
		return
	}
	if p.runError != nil {
		// Exited at start
		s := p.out.String()
		var errorMessage string
		if m := panicRe.FindStringSubmatch(s); len(m) > 1 {
			errorMessage = m[1]
		}
		data := map[string]interface{}{
			"Project":  p,
			"Error":    errorMessage,
			"ExitCode": p.exitCode,
			"Output":   uncolor(s),
			"Started":  formatTime(p.started),
		}
		ctx.MustExecute("exited.html", data)
		return
	}
	if p.proxy == nil {
		// Building
		if ctx.R.Method != "POST" || ctx.R.Method != "PUT" {
			data := map[string]interface{}{
				"Project": p,
				"Name":    p.Name(),
				"Built":   formatTime(p.built),
				"Started": "1", // This waits until the app is restarted for reloading
			}
			ctx.MustExecute("building.html", data)
			return
		}
		// Wait until the app starts
		for {
			time.Sleep(10 * time.Millisecond)
			if p.proxy != nil {
				break
			}
		}
	}
	// Proxy
	p.proxy.ServeHTTP(ctx, ctx.R)
}

func (p *Project) StatusHandler(ctx *app.Context) {
	built := formatTime(p.built)
	started := formatTime(p.started)
	ctx.WriteJSON(map[string]interface{}{
		"built":   built,
		"started": started,
	})
}

func (p *Project) waitForBuild() {
	for {
		p.Lock()
		if p.buildCmd == nil {
			p.Unlock()
			break
		}
		p.Unlock()
	}
}

func (p *Project) isRunning() bool {
	return len(p.errors) == 0 && p.runError == nil
}

func findConfig(dir string, name string) string {
	if name == "" {
		if c := findConfig(dir, devConfigName); c != "" {
			return c
		}
		name = config.DefaultName
	}
	configPath := filepath.Join(dir, name)
	for _, v := range []string{configPath, name} {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	return ""
}

func Dev(ctx *app.Context) {
	var verbose bool
	ctx.ParseParamValue("v", &verbose)
	if !verbose {
		log.SetLevel(log.LInfo)
	}
	var dir string
	var configName string
	var noBrowser bool
	ctx.ParseParamValue("dir", &dir)
	ctx.ParseParamValue("config", &configName)
	path, err := filepath.Abs(dir)
	if err != nil {
		log.Panic(err)
	}
	configPath := findConfig(dir, configName)
	if configPath == "" {
		log.Panicf("can't find configuration file %s in %s", configName, dir)
	}
	log.Infof("Using config file %s", configPath)
	p := NewProject(path, configPath)
	ctx.ParseParamValue("port", &p.port)
	ctx.ParseParamValue("tags", &p.tags)
	ctx.ParseParamValue("race", &p.race)
	ctx.ParseParamValue("no-debug", &p.noDebug)
	ctx.ParseParamValue("no-cache", &p.noCache)
	ctx.ParseParamValue("profile", &p.profile)
	ctx.ParseParamValue("no-browser", &noBrowser)
	clean(dir)
	go p.Build()
	eof := "C"
	if runtime.GOOS == "windows" {
		eof = "Z"
	}
	log.Infof("Starting Gondola development server on port %d (press Control+%s to exit)", p.port, eof)
	if !noBrowser {
		time.AfterFunc(time.Second, func() {
			startBrowser(fmt.Sprintf("http://localhost:%d", p.port))
		})
	}
	p.Listen()
}

func startBrowser(url string) bool {
	// try to start the browser
	var args []string
	switch runtime.GOOS {
	case "darwin":
		args = []string{"open"}
	case "windows":
		args = []string{"cmd", "/c", "start"}
	default:
		args = []string{"xdg-open"}
	}
	cmd := exec.Command(args[0], append(args[1:], url)...)
	return cmd.Start() == nil
}

func init() {
	admin.Register(Dev, &admin.Options{
		Help: "Starts the development server",
		Flags: admin.Flags(
			admin.StringFlag("dir", ".", "Directory of the project"),
			admin.StringFlag("config", "", "Configuration name to use - if empty the following are tried, in order "+devConfigName+", "+config.DefaultName),
			admin.StringFlag("tags", "", "Go build tags to pass to the compiler"),
			admin.BoolFlag("no-debug", false, "Disable AppDebug, TemplateDebug and LogDebug - see gnd.la/config for details"),
			admin.BoolFlag("no-cache", false, "Disables the cache when running the project"),
			admin.BoolFlag("profile", false, "Compiles and runs the project with profiling enabled"),
			admin.IntFlag("port", 8888, "Port to listen on"),
			admin.BoolFlag("race", false, "Enable -race when building. If the platform does not support -race, this option is ignored."),
			admin.BoolFlag("no-brower", false, "Don't open the default browser when starting the development server."),
			admin.BoolFlag("v", false, "Enable verbose output"),
		),
	})
}
