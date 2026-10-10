package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/gateway"
)

// `xdev gateway` — the Telegram bridge (M15 services tail). One daemon that
// long-polls the Bot API and runs each turn as a detached worker process of
// this same binary; see internal/gateway for why.
//
// Subcommands:
//
//	xdev gateway              run in the foreground (same as `run`)
//	xdev gateway setup        interactive install: token, allowlist, memory, service
//	xdev gateway start|stop   background daemon via the OS service manager
//	xdev gateway status|logs  what it is doing
//	xdev gateway run          the daemon itself (what the service unit runs)
//
// The service manager is launchd on macOS and systemd --user on Linux, so
// "run in the background" means the OS supervises it rather than a fork that
// dies with the terminal.

const gatewayUsage = `xdev gateway — talk to xdev from Telegram

  xdev gateway setup            install: token, allowlist, memory, background service
  xdev gateway start            start the background daemon (launchd/systemd)
  xdev gateway stop             stop it
  xdev gateway status           bot, chats, pending turns, service state
  xdev gateway logs             tail the daemon log
  xdev gateway run              run in the foreground (what the service runs)
  xdev gateway send <chat> <text>   send a message without the model (test)

Config lives in ` + "`gateway:`" + ` in config.yml (allowedChats, allowedAll,
workspace, pollTimeout, leaseWait, replyChunk, model). The bot token never
does: it comes from TELEGRAM_BOT_TOKEN or <data dir>/gateway.token (0600),
which ` + "`xdev gateway setup`" + ` writes.
`

func runGateway(args []string, version string) int {
	sub := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "run":
		return gatewayRun(args, version)
	case "setup":
		return gatewaySetup(args, version)
	case "start":
		return gatewayStart(args, version)
	case "stop":
		return gatewayStop(args)
	case "status":
		return gatewayStatus(args)
	case "logs":
		return gatewayLogs(args)
	case "send":
		return gatewaySend(args)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, gatewayUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "xdev gateway: unknown subcommand %q\n\n%s", sub, gatewayUsage)
		return 2
	}
}

// gatewayRun is the daemon loop (the service unit's ExecStart).
func gatewayRun(args []string, version string) int {
	fs := flag.NewFlagSet("gateway run", flag.ContinueOnError)
	logFile := fs.String("log", "", "append lifecycle lines to this file as well as stderr")
	apiURL := fs.String("api", "", "Bot API base URL (default https://api.telegram.org; tests and a local bridge point it elsewhere)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, "usage: xdev gateway run [--log FILE] [--api URL]\n") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if isGatewayWorker() {
		// A worker spawned by a gateway must never become a second poller:
		// two pollers on one token is the 409 Conflict the daemon reports.
		fmt.Fprintln(os.Stderr, "xdev gateway: refusing to start a poller inside a gateway turn")
		return 2
	}
	dataDir := config.DataDir()
	settings := lastSettings()
	logf, cleanup := gatewayLogger(*logFile)
	defer cleanup()

	d, err := gateway.NewDaemon(gateway.Options{
		Settings: settings,
		Version:  version,
		DataDir:  dataDir,
		Logf:     logf,
		BaseURL:  *apiURL,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return 2
	}
	if err := gateway.WritePID(dataDir); err != nil {
		logf("gateway: pid file: %v", err)
	}
	defer gateway.RemovePID(dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf("gateway %s starting (token %s, data %s)", version, gateway.MaskToken(gateway.ResolveToken(dataDir)), dataDir)
	if err := d.Run(ctx); err != nil {
		logf("gateway: %v", err)
		fmt.Fprintln(os.Stderr, "xdev:", err)
		return 1
	}
	logf("gateway: stopped")
	return 0
}

// gatewayLogger returns the log sink: the file when --log names one (the
// service unit does), with every line also on stderr so `gateway run` in a
// terminal is readable.
func gatewayLogger(path string) (func(string, ...any), func()) {
	if path == "" {
		return func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "xdev gateway: "+format+"\n", args...)
		}, func() {}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "xdev gateway: log dir:", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// Fall back to stderr rather than dying: losing the log file must
		// not take the bridge down with it.
		fmt.Fprintln(os.Stderr, "xdev gateway: log file:", err)
		return func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "xdev gateway: "+format+"\n", args...)
		}, func() {}
	}
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		ts := time.Now().Format("2006-01-02 15:04:05")
		fmt.Fprintf(f, "%s %s\n", ts, line)
		fmt.Fprintf(os.Stderr, "%s %s\n", ts, line)
	}, func() { _ = f.Close() }
}

// isGatewayWorker reports whether this process was spawned by a gateway
// turn (set by internal/gateway's spawn).
func isGatewayWorker() bool { return os.Getenv("XDEV_GATEWAY_CHAT") != "" }

// --- setup -----------------------------------------------------------------

// gatewaySetup is the installer: it asks the four things a bridge needs
// (token, allowlist, workspace, memory backend), writes them where they
// belong, verifies the token against the Bot API, and offers to install the
// background service. It is deliberately interactive but every question has
// a default, so accepting everything is a working install.
func gatewaySetup(args []string, version string) int {
	fs := flag.NewFlagSet("gateway setup", flag.ContinueOnError)
	tokenFlag := fs.String("token", "", "bot token from @BotFather (skips the prompt)")
	chatsFlag := fs.String("chats", "", "comma-separated allowed chat ids")
	allowAll := fs.Bool("allow-all", false, "accept any chat (single-user box)")
	workspace := fs.String("workspace", "", "directory gateway turns run in")
	leankgURL := fs.String("leankg", "", "LeanKG REST base URL for memory (e.g. http://127.0.0.1:9700)")
	service := fs.Bool("service", false, "install the background service without asking")
	noService := fs.Bool("no-service", false, "do not install a background service")
	yes := fs.Bool("yes", false, "accept every default (non-interactive)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: xdev gateway setup [--token T] [--chats ID,ID] [--workspace DIR] [--leankg URL] [--service|--no-service]\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dataDir := config.DataDir()
	in := bufio.NewReader(os.Stdin)
	interactive := !*yes && isStdinTTY()

	fmt.Printf("xdev gateway setup\n\n")

	// 1. Token.
	token := strings.TrimSpace(*tokenFlag)
	if token == "" {
		token = gateway.ReadTokenFile(dataDir)
		if token != "" {
			fmt.Printf("bot token: found in %s (token %s)\n", gateway.TokenFilePath(dataDir), gateway.MaskToken(token))
		}
	}
	if token == "" {
		if !interactive {
			fmt.Fprintln(os.Stderr, "xdev gateway setup: no token — pass --token, set TELEGRAM_BOT_TOKEN, or run interactively")
			return 2
		}
		fmt.Print("1. Bot token from @BotFather (or /newbot to make one): ")
		token = readLine(in)
	}
	if !gateway.ValidTokenShape(token) {
		fmt.Fprintln(os.Stderr, "xdev gateway setup: that does not look like a bot token (want 123456789:AA…)")
		return 2
	}
	if err := gateway.WriteToken(dataDir, token); err != nil {
		fmt.Fprintln(os.Stderr, "xdev gateway setup:", err)
		return 2
	}
	fmt.Printf("   saved to %s (mode 0600)\n", gateway.TokenFilePath(dataDir))

	// Verify the token now: a wrong token discovered by a daemon that
	// silently polls nothing is the setup failure worth spending a second on.
	// Verify the token against the real API. A failure is a WARNING, not a
	// stop: the token check needs the network, and refusing to finish an
	// install because a plane had its Wi-Fi off is a bad trade. The daemon
	// verifies again at startup and stops there.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tg := &gateway.Telegram{Token: token}
	if who, merr := tg.GetMe(ctx); merr != nil {
		fmt.Printf("   NOTE: could not reach the Bot API to verify the token (%v)\n", merr)
		fmt.Println("   continuing — `xdev gateway run` checks it again and stops if it is wrong")
	} else {
		fmt.Printf("   connected as @%s (id %d)\n", who.Username, who.ID)
	}

	// 2. Allowlist.
	chats := strings.TrimSpace(*chatsFlag)
	if chats == "" {
		chats = gateway.FormatChatList(lastSettings().GatewayAllowedChats())
	}
	if chats == "" && !*allowAll {
		if interactive {
			fmt.Println("\n2. Which chat ids may talk to it?")
			fmt.Println("   Send /whoami to the bot from the phone you want to allow — it")
			fmt.Println("   prints the chat id even when it is not allowed yet.")
			fmt.Print("   Allowed chat ids (comma separated, blank = allow nobody): ")
			chats = strings.TrimSpace(readLine(in))
		} else {
			fmt.Println("\n2. allowlist: none — every message will be refused until you set gateway.allowedChats")
		}
	} else if chats != "" {
		fmt.Printf("\n2. allowlist: %s\n", chats)
	}

	// 3. Workspace.
	ws := strings.TrimSpace(*workspace)
	if ws == "" {
		if s := lastSettings().Gateway.Workspace; strings.TrimSpace(s) != "" {
			ws = s
		} else if interactive {
			fmt.Printf("\n3. Directory gateway turns run in [%s]: ", config.DefaultGatewayWorkspace())
			ws = strings.TrimSpace(readLine(in))
		}
	}
	if ws == "" {
		ws = config.DefaultGatewayWorkspace()
	}
	fmt.Printf("\n3. workspace: %s\n", ws)

	// 4. Memory. LeanKG speaks the wire the hindsight backend already
	// uses, so "memory backend with leankg" is a URL, not a new backend.
	memURL := strings.TrimSpace(*leankgURL)
	if memURL == "" {
		if s := lastSettings(); s.Memory == "hindsight" && s.Hindsight.APIURL != "" {
			memURL = s.Hindsight.APIURL
		} else if interactive {
			fmt.Println("\n4. Memory backend — LeanKG makes the agent keep what it learns.")
			fmt.Print("   LeanKG REST base URL (blank = local backend, MEMORY.md only): ")
			memURL = strings.TrimSpace(readLine(in))
		}
	}
	if memURL != "" {
		fmt.Printf("\n4. memory: LeanKG at %s\n", memURL)
	} else {
		fmt.Println("\n4. memory: local (MEMORY.md under the data dir)")
	}

	// Write the settings: config.set validates the key path and refuses a
	// file the next start would reject, so the writer is the CLI itself.
	path := config.GlobalSettingsPath()
	set := func(key, value string) bool {
		if err := config.Set(path, key, value); err != nil {
			fmt.Fprintf(os.Stderr, "xdev gateway setup: %s: %v\n", key, err)
			return false
		}
		return true
	}
	ok := set("gateway.enabled", "true")
	ok = set("gateway.workspace", ws) && ok
	if *allowAll {
		ok = set("gateway.allowedAll", "true") && ok
	} else if chats != "" {
		// Written as a list, not a comma string: the key is []string, and a
		// scalar would be rejected by the next start (the round-trip guard).
		ids, perr := gateway.ParseChatList(chats)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "xdev gateway setup:", perr)
			return 2
		}
		// `config set` parses its value as a YAML scalar, and this key is a
		// list, so setup writes the list itself — one id per line under
		// gateway.allowedChats. The round-trip guard is the same one
		// config.Set applies.
		if err := writeAllowedChats(path, ids); err != nil {
			fmt.Fprintln(os.Stderr, "xdev gateway setup:", err)
			return 2
		}
	}
	if memURL != "" {
		ok = set("memory", "hindsight") && ok
		ok = set("hindsight.apiUrl", memURL) && ok
		// The gateway turns run in the workspace, not in a repo, so a
		// project-tagged scope would be one tag for the whole bridge.
		ok = set("hindsight.scoping", "global") && ok
		ok = set("hindsight.bankId", "xdev-gateway") && ok
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "\nsome settings could not be written; fix the errors above and re-run setup")
		return 1
	}
	fmt.Printf("\nsettings written to %s\n", path)

	// 5. Service.
	install := *service
	if !*service && !*noService && interactive {
		fmt.Print("\n5. Install the background service so it starts at login? [Y/n] ")
		ans := strings.ToLower(strings.TrimSpace(readLine(in)))
		install = ans == "" || ans == "y" || ans == "yes"
	}
	if install {
		if code := gatewayServiceInstall(false, version); code != 0 {
			return code
		}
	} else {
		fmt.Println("\n5. service: not installed — run `xdev gateway run` in the foreground,")
		fmt.Println("   or `xdev gateway start` for the background service.")
	}
	fmt.Println("\ntry it: message the bot on Telegram. `/help` lists the commands.")
	return 0
}

// gatewayUninstall stops the daemon and removes the service unit, without
// touching the token or the sessions: uninstalling the service must not throw
// away the bridge's conversations.
func gatewayUninstall(args []string) int {
	fs := flag.NewFlagSet("gateway uninstall", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dataDir := config.DataDir()
	if pid := gateway.ReadPID(dataDir); pid > 0 && processAlive(pid) {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	_ = gateway.StopUnit()
	path, err := gateway.RemoveUnit()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev gateway:", err)
		return 1
	}
	gateway.RemovePID(dataDir)
	fmt.Printf("service removed (%s)\n", path)
	fmt.Println("the bot token, chat map and sessions were left in place; remove them by hand if you want them gone.")
	return 0
}

// gatewayServiceInstall writes and loads the OS service definition.
func gatewayServiceInstall(quiet bool, version string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev gateway:", err)
		return 1
	}
	path, err := gateway.WriteUnit(gateway.InstallOptions{
		Exe:     exe,
		DataDir: config.DataDir(),
		Version: version,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "xdev gateway: %v\n", err)
		fmt.Fprintf(os.Stderr, "  the unit is at %s — load it by hand\n", path)
		return 1
	}
	if !quiet {
		fmt.Printf("\n5. wrote %s\n   supervisor %s\n", path, supervisorName())
		fmt.Println("   service installed and started")
	}
	return 0
}

// supervisorName names the service manager this OS uses.
func supervisorName() string {
	if runtime.GOOS == "linux" {
		return "systemd --user"
	}
	return "launchd"
}

// gatewayStart installs/loads the service (idempotent).
func gatewayStart(args []string, version string) int {
	fs := flag.NewFlagSet("gateway start", flag.ContinueOnError)
	foreground := fs.Bool("foreground", false, "run in this terminal instead of the service manager")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *foreground {
		return gatewayRun(nil, version)
	}
	return gatewayServiceInstall(false, version)
}

// gatewayStop asks the service manager to stop it, and as a fallback signals
// the recorded pid.
func gatewayStop(args []string) int {
	if uerr := gateway.StopUnit(); uerr == nil {
		fmt.Println("gateway stopped (service manager)")
		return 0
	} else if os.Getenv("XDEV_GATEWAY_VERBOSE") != "" {
		fmt.Fprintf(os.Stderr, "  service manager: %v\n", uerr)
	}
	pid := gateway.ReadPID(config.DataDir())
	if pid <= 0 {
		fmt.Fprintln(os.Stderr, "xdev gateway: no service and no pid file — is it running?")
		return 1
	}
	p, perr := os.FindProcess(pid)
	if perr != nil {
		fmt.Fprintln(os.Stderr, "xdev gateway:", perr)
		return 1
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintln(os.Stderr, "xdev gateway:", err)
		return 1
	}
	gateway.RemovePID(config.DataDir())
	fmt.Printf("gateway stopped (pid %d)\n", pid)
	return 0
}

// gatewayStatus reports what the daemon is configured to do and whether it
// is running, without needing it to be up.
func gatewayStatus(args []string) int {
	dataDir := config.DataDir()
	settings := lastSettings()

	fmt.Println("xdev gateway")
	fmt.Printf("  enabled:     %v\n", settings.GatewayEnabled())
	fmt.Printf("  workspace:   %s\n", settings.GatewayWorkspace())
	if settings.GatewayAllowAll() {
		fmt.Println("  allowlist:   every chat (gateway.allowedAll)")
	} else if chats := settings.GatewayAllowedChats(); len(chats) > 0 {
		fmt.Printf("  allowlist:   %s\n", gateway.FormatChatList(chats))
	} else {
		fmt.Println("  allowlist:   none — every message is refused until gateway.allowedChats is set")
	}
	fmt.Printf("  memory:      %s\n", gatewayMemoryNote(settings))
	fmt.Printf("  token:       %s\n", gatewayTokenNote(dataDir))

	sess, err := gateway.OpenSessions(dataDir)
	if err == nil {
		fmt.Printf("  chats:       %d with a session (%s)\n", sess.Count(), sess.Path())
	} else {
		fmt.Printf("  chats:       unreadable: %v\n", err)
	}

	if pid := gateway.ReadPID(dataDir); pid > 0 && processAlive(pid) {
		fmt.Printf("  daemon:      running (pid %d)\n", pid)
	} else {
		fmt.Println("  daemon:      not running")
	}
	fmt.Printf("  log:         %s\n", gateway.LogPath(dataDir))

	if path := gateway.UnitPath(); path != "" {
		if _, serr := os.Stat(path); serr == nil {
			fmt.Printf("  service:     installed (%s)%s\n", path, loadedNote(gateway.UnitLoaded()))
		} else {
			fmt.Printf("  service:     not installed (run `xdev gateway setup`)\n")
		}
	}
	return 0
}

// gatewayMemoryNote names the memory backend the gateway turns will use.
func gatewayMemoryNote(s *config.Settings) string {
	if s == nil || s.Memory == "" {
		return "local (default)"
	}
	if s.Memory == "hindsight" {
		url := s.Hindsight.APIURL
		if url == "" {
			url = "http://localhost:8888"
		}
		return fmt.Sprintf("hindsight — LeanKG at %s, bank %q, scope %q", url, s.Hindsight.BankID, s.Hindsight.Scoping)
	}
	return s.Memory
}

func gatewayTokenNote(dataDir string) string {
	if v := strings.TrimSpace(os.Getenv(gateway.TokenEnv)); v != "" {
		return gateway.MaskToken(v) + " (from " + gateway.TokenEnv + ")"
	}
	if v := gateway.ReadTokenFile(dataDir); v != "" {
		return gateway.MaskToken(v) + " (from " + gateway.TokenFilePath(dataDir) + ")"
	}
	return "NONE — run `xdev gateway setup`"
}

// gatewayLogs tails the daemon log.
func gatewayLogs(args []string) int {
	fs := flag.NewFlagSet("gateway logs", flag.ContinueOnError)
	lines := fs.Int("lines", 100, "how many trailing lines")
	follow := fs.Bool("f", false, "follow the log")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path := gateway.LogPath(config.DataDir())
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xdev gateway: no log at %s (%v)\n", path, err)
		return 1
	}
	parts := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(parts) > *lines {
		parts = parts[len(parts)-*lines:]
	}
	fmt.Println(strings.Join(parts, "\n"))
	if !*follow {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer f.Close()
	_, _ = f.Seek(int64(len(raw)), io.SeekStart)
	for {
		line, rerr := bufio.NewReader(f).ReadString('\n')
		if line != "" {
			fmt.Print(line)
		}
		if rerr != nil {
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// gatewaySend posts a plain message to a chat through the Bot API. It is the
// one command that proves the token and the chat id work without involving
// the model, which is exactly what a broken install needs first.
func gatewaySend(args []string) int {
	fs := flag.NewFlagSet("gateway send", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: xdev gateway send <chat-id> <text>")
		return 2
	}
	chatID, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdev gateway send: chat id must be numeric")
		return 2
	}
	dataDir := config.DataDir()
	token := gateway.ResolveToken(dataDir)
	if token == "" {
		fmt.Fprintln(os.Stderr, "xdev gateway send: no token — run `xdev gateway setup`")
		return 2
	}
	tg := &gateway.Telegram{Token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	text := strings.Join(rest[1:], " ")
	for _, chunk := range gateway.ChunkText(text, lastSettings().GatewayReplyChunk()) {
		if err := tg.SendMessage(ctx, chatID, chunk); err != nil {
			fmt.Fprintln(os.Stderr, "xdev gateway send:", err)
			return 1
		}
	}
	fmt.Printf("sent %d chunk(s) to %d\n", len(gateway.ChunkText(text, lastSettings().GatewayReplyChunk())), chatID)
	return 0
}

// writeAllowedChats merges gateway.allowedChats into the settings file as a
// YAML block list. config.Set cannot express a list value (its value is a
// scalar), so this is the one write that bypasses it — with the same
// round-trip check, so a file it produces is always one the next start
// accepts.
func writeAllowedChats(path string, ids []int64) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	doc := map[string]any{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s: refusing to edit unparseable file: %w", path, err)
		}
	}
	gw, ok := doc["gateway"].(map[string]any)
	if !ok {
		gw = map[string]any{}
		doc["gateway"] = gw
	}
	list := make([]any, 0, len(ids))
	for _, id := range ids {
		list = append(list, id)
	}
	gw["allowedChats"] = list
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	// The same guard config.Set applies: decode as Settings with
	// KnownFields(true), so a bad write fails here, not at the next start.
	var probe config.Settings
	d := yaml.NewDecoder(bytes.NewReader(out))
	d.KnownFields(true)
	if err := d.Decode(&probe); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: refusing to write a file the next start would reject: %w", path, err)
	}
	if err := config.WriteSettingsAtomicFor(path, out); err != nil {
		return err
	}
	return nil
}

// readLine reads one line, tolerating EOF (a piped setup script). An empty
// result means "take the default".
func readLine(r *bufio.Reader) string {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// isStdinTTY reports whether stdin is a terminal, so a piped run does not
// hang on a prompt nobody can answer.
func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// loadedNote marks whether the supervisor has the unit loaded now.
func loadedNote(loaded bool) string {
	if loaded {
		return " — running"
	}
	return " — not loaded"
}

// processAlive reports whether pid is a live process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}
