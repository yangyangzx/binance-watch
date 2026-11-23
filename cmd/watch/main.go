// watch：币价哨兵 CLI —— 增删查规则、看实时状态。daemon 会自动热加载，改完无需重启。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"binance-watch/internal/engine"
	"binance-watch/internal/price"
	"binance-watch/internal/rules"
)

var (
	dataDir string
	webAddr string
	useCol  bool
)

func main() {
	home, _ := os.UserHomeDir()
	def := os.Getenv("BW_DATA")
	if def == "" {
		def = filepath.Join(home, "go", "binance-watch", "data")
	}
	web := os.Getenv("BW_WEB")
	if web == "" {
		web = "127.0.0.1:8090"
	}
	flag.StringVar(&dataDir, "data", def, "数据目录（$BW_DATA）")
	flag.StringVar(&webAddr, "web", web, "watchd 面板地址，用于 status（$BW_WEB）")
	noColor := flag.Bool("no-color", false, "关闭彩色输出")
	flag.Usage = usage
	flag.Parse()
	useCol = !*noColor && os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout)

	args := flag.Args()
	if len(args) == 0 {
		usage()
		return
	}
	cmd, rest := args[0], args[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage()
		return
	}
	rl, err := rules.New(dataDir)
	if err != nil {
		die("打开规则失败: %v", err)
	}
	switch cmd {
	case "add":
		cmdAdd(rl, rest)
	case "list", "ls":
		cmdList(rl)
	case "remove", "rm", "del":
		cmdRemove(rl, rest)
	case "enable", "active", "on":
		cmdActive(rl, rest, true)
	case "disable", "deactivate", "off":
		cmdActive(rl, rest, false)
	case "status", "st":
		cmdStatus(rl)
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`binance-watch CLI —— 币价提醒规则管理（daemon 自动热加载，无需重启）

用法:
  watch [-data DIR] <命令> ...

命令:
  add     添加规则（见下）
  list    列出规则                           (别名 ls)
  status  实时行情 + 规则距触发还有多远        (需要 watchd 正在运行，别名 st)
  rm ID   删除规则（ID 可只写唯一前缀）       (别名 remove/del)
  off ID  停用规则                           (别名 disable/deactivate)
  on  ID  启用规则；一次性规则会重新上膛      (别名 enable/active)

添加规则:
  到价     watch add BTCUSDT above 70000 [--once]
           watch add BTCUSDT --target 70000 --dir above
  涨跌幅   watch add ETHUSDT --pct 3% --window 1h --dir down
           watch add DOTUSDT --pct 5% --window 4h              (默认双向 auto)
  接近     watch add SOLUSDT --near 0.5% --target 150

选项:
  --pct   涨跌幅阈值：写 3% / 3.5%，或纯整数 = 基点（300 = 3.00%，兼容旧用法）
  --near  距目标的距离：写 0.5%，或纯整数 = 基点（50 = 0.50%）
  --window 窗口：5 / 15m / 1h / 4h（最大 4h，默认按 4h 整窗）
  --dir   到价: above|below；涨跌幅: up|down|auto
  --cooldown 同一规则两次提醒的最小间隔，默认 30（分钟，也可写 2h）
  --once  触发一次后自动停用
  --note  备注（会出现在邮件里）

全局选项:
`)
	flag.PrintDefaults()
}

// ---------------------------------------------------------------- 输出工具

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func paint(code, s string) string {
	if !useCol {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func dim(s string) string    { return paint("2", s) }
func bold(s string) string   { return paint("1", s) }
func green(s string) string  { return paint("32", s) }
func red(s string) string    { return paint("31", s) }
func yellow(s string) string { return paint("33", s) }

func die(f string, a ...any) {
	fmt.Fprintln(os.Stderr, red("错误: ")+fmt.Sprintf(f, a...))
	os.Exit(1)
}

// width 终端显示宽度：CJK 全角字符占 2 列。
func width(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r < 0x80:
			w++
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r),
			unicode.Is(unicode.Hangul, r), r >= 0xFF00 && r <= 0xFFEF, r >= 0x3000 && r <= 0x303F:
			w += 2
		default:
			w++
		}
	}
	return w
}

// pad 按显示宽度右侧补空格；colored 为已着色文本（plain 是它的无色版本，用来量宽度）。
func pad(colored, plain string, w int) string {
	if n := w - width(plain); n > 0 {
		return colored + strings.Repeat(" ", n)
	}
	return colored
}

func pctColor(bp int64, ok bool) string {
	if !ok {
		return dim("   --  ")
	}
	t := fmt.Sprintf("%7s", price.PctSigned(bp))
	switch {
	case bp > 0:
		return green(t)
	case bp < 0:
		return red(t)
	}
	return t
}

// ---------------------------------------------------------------- 参数解析

// parseBP 解析 "3%" / "3.5%" / "300"（纯整数 = 基点）。
// % 分支走 price 的精确十进制定点解析（不经 float64）：
// 二进制浮点曾把 "2.675%" 算成 267.4999… → 舍入成 267（应为 268）；
// 并且四舍五入后为 0 的量（< 0.005%）明确报错，而不是静默返回 0 被当成"参数未填"。
func parseBP(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "%") {
		p, err := price.Parse(strings.TrimSpace(strings.TrimSuffix(s, "%")))
		if err != nil || p <= 0 {
			return 0, fmt.Errorf("百分比 %q 不合法", s)
		}
		const fixedPerBP = price.Scale / 100 // 1bp = 0.01% → 定点 1e6
		bp := (int64(p) + fixedPerBP/2) / fixedPerBP
		if bp <= 0 {
			return 0, fmt.Errorf("百分比 %q 太小（最小 1 个基点 = 0.01%%）", s)
		}
		return int(bp), nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q 不合法：写 3%% 或整数基点（300 = 3.00%%）", s)
	}
	return n, nil
}

// parseMinutes 解析 "90" / "30m" / "2h" / "1d" 为分钟。
func parseMinutes(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "d"):
		mult, s = 1440, strings.TrimSuffix(s, "d")
	case strings.HasSuffix(s, "h"):
		mult, s = 60, strings.TrimSuffix(s, "h")
	case strings.HasSuffix(s, "m"):
		s = strings.TrimSuffix(s, "m")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("时长不合法（示例：30、30m、2h）")
	}
	return n * mult, nil
}

// splitLeading 把命令行里开头的非 flag 参数（位置参数）与后面的 flag 分开，
// 这样 `add BTCUSDT above 70000 --once` 与 `add --once BTCUSDT ...` 都能工作。
func splitLeading(args []string) (pos, flags []string) {
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		i++
	}
	return args[:i], args[i:]
}

func cmdAdd(rl *rules.Store, args []string) {
	pos, flagArgs := splitLeading(args)
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sym := fs.String("sym", "", "")
	target := fs.String("target", "", "")
	dir := fs.String("dir", "", "")
	pct := fs.String("pct", "", "")
	near := fs.String("near", "", "")
	window := fs.String("window", "", "")
	cooldown := fs.String("cooldown", "30", "")
	once := fs.Bool("once", false, "")
	note := fs.String("note", "", "")
	if err := fs.Parse(flagArgs); err != nil {
		die("参数错误: %v（watch help 查看用法）", err)
	}
	pos = append(pos, fs.Args()...)

	// 位置参数简写：SYMBOL [above|below PRICE]
	if *sym == "" && len(pos) > 0 {
		*sym, pos = pos[0], pos[1:]
	}
	if len(pos) >= 2 && (pos[0] == "above" || pos[0] == "below") && *target == "" && *pct == "" && *near == "" {
		*dir, *target, pos = pos[0], pos[1], pos[2:]
	}
	if len(pos) > 0 {
		die("多余的参数: %s", strings.Join(pos, " "))
	}
	if *sym == "" {
		die("需要交易对，例如: watch add BTCUSDT above 70000")
	}

	pctBP, err := parseBP(*pct)
	if err != nil {
		die("--pct: %v", err)
	}
	nearBP, err := parseBP(*near)
	if err != nil {
		die("--near: %v", err)
	}
	win, err := parseMinutes(*window)
	if err != nil {
		die("--window: %v", err)
	}
	cd, err := parseMinutes(*cooldown)
	if err != nil {
		die("--cooldown: %v", err)
	}

	r := rules.Rule{Symbol: *sym, Target: strings.ReplaceAll(*target, ",", ""), Dir: *dir, PctBP: pctBP, NearBP: nearBP,
		WindowMin: win, CooldownMin: cd, Once: *once, Note: *note}
	switch {
	case pctBP > 0 && nearBP > 0:
		die("--pct 与 --near 只能二选一")
	case pctBP > 0:
		r.Trigger = rules.TrigPct
	case nearBP > 0:
		r.Trigger = rules.TrigNear
	default:
		r.Trigger = rules.TrigTarget
		if r.Dir == "" {
			die("到价规则需要方向: watch add %s above|below <价格>", strings.ToUpper(*sym))
		}
	}
	added, err := rl.Add(r)
	if err != nil {
		die("%v", err)
	}
	fmt.Println(green("✔ 已添加"), bold(added.ID), added.Symbol, added.Describe(), dim(added.Flags()))
	fmt.Println(dim("  watchd 运行中会在 ~2 秒内自动加载，无需重启。"))
}

func ruleState(r rules.Rule) (plain string, colored string) {
	switch {
	case !r.Active && r.Once && r.HitCount > 0:
		return "已完成", dim("已完成")
	case !r.Active:
		return "已停用", dim("已停用")
	}
	return "监控中", green("监控中")
}

func cmdList(rl *rules.Store) {
	list := rl.List()
	if len(list) == 0 {
		fmt.Println(dim("(还没有规则。试试: watch add BTCUSDT above 70000)"))
		return
	}
	fmt.Println(bold(pad("ID", "ID", 12) + pad("状态", "状态", 8) + pad("交易对", "交易对", 12) + pad("类型", "类型", 8) + pad("条件", "条件", 26) + "其它"))
	for _, r := range list {
		sp, sc := ruleState(r)
		extra := r.Flags()
		if r.HitCount > 0 {
			extra += fmt.Sprintf("  命中 %d 次，最近 %s", r.HitCount, r.LastHitAt.Local().Format("01-02 15:04"))
		}
		if r.Note != "" {
			extra += "  # " + r.Note
		}
		desc := r.Describe()
		fmt.Println(pad(r.ID, r.ID, 12) + pad(sc, sp, 8) + pad(r.Symbol, r.Symbol, 12) + pad(r.TypeLabel(), r.TypeLabel(), 8) + pad(desc, desc, 26) + dim(strings.TrimSpace(extra)))
	}
}

func cmdRemove(rl *rules.Store, args []string) {
	if len(args) < 1 {
		die("需要规则 ID（watch list 查看）")
	}
	for _, id := range args {
		r, err := rl.Remove(id)
		if err != nil {
			die("%v", err)
		}
		fmt.Println(green("✔ 已删除"), r.ID, r.Symbol, r.Describe())
	}
}

func cmdActive(rl *rules.Store, args []string, on bool) {
	if len(args) < 1 {
		die("需要规则 ID（watch list 查看）")
	}
	for _, id := range args {
		r, err := rl.SetActive(id, on)
		if err != nil {
			die("%v", err)
		}
		if on {
			fmt.Println(green("✔ 已启用"), r.ID, r.Symbol, r.Describe())
		} else {
			fmt.Println(yellow("✔ 已停用"), r.ID, r.Symbol, r.Describe())
		}
	}
}

// ---------------------------------------------------------------- status

func fetchStatus() (*engine.Status, error) {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + webAddr + "/api/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var st engine.Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func cmdStatus(rl *rules.Store) {
	st, err := fetchStatus()
	if err != nil {
		fmt.Println(yellow("无法连接 watchd 面板 ") + dim("("+webAddr+")") + "：" + err.Error())
		fmt.Println(dim("  确认 watchd 在运行且没有用 -web \"\" 关闭面板；下面仅列出规则文件内容。\n"))
		cmdList(rl)
		return
	}
	ws := red("● WS 断开")
	if st.WS {
		ws = green("● WS 已连接")
	}
	feed := green("● 行情正常")
	okN := 0
	for _, x := range st.Symbols {
		if x.State == "ok" {
			okN++
		}
	}
	if st.Down {
		feed = red("● 行情断流")
	} else if okN == 0 {
		feed = yellow("● 等待行情")
	}
	fmt.Printf("%s  %s  %s\n\n", ws, feed,
		dim(fmt.Sprintf("运行 %s · 重连 %d 次 · 待发邮件 %d", (time.Duration(st.UptimeSec)*time.Second).String(), st.Reconnects, st.Pending)))

	fmt.Println(bold(pad("交易对", "交易对", 12) + pad("现价", "现价", 16) + "   5m      15m     1h      4h    更新"))
	for _, x := range st.Symbols {
		priceTxt := x.Price
		if priceTxt == "" {
			priceTxt = "--"
		}
		line := pad(x.Symbol, x.Symbol, 12) + pad(bold(priceTxt), priceTxt, 16)
		for _, c := range x.Changes {
			line += " " + pctColor(c.BP, c.OK)
		}
		switch x.State {
		case "ok":
			line += dim(fmt.Sprintf("  %ds 前", x.AgeSec))
		case "stale":
			line += yellow(fmt.Sprintf("  %ds 前（过期）", x.AgeSec))
		case "invalid":
			line = pad(x.Symbol, x.Symbol, 12) + red("Binance 不存在该交易对")
		default:
			line += yellow("  等待数据")
		}
		fmt.Println(line)
	}

	fmt.Println()
	if len(st.Rules) == 0 {
		fmt.Println(dim("(没有规则)"))
		return
	}
	fmt.Println(bold(pad("ID", "ID", 12) + pad("状态", "状态", 14) + pad("交易对", "交易对", 12) + pad("条件", "条件", 26) + "距触发"))
	for _, r := range st.Rules {
		var sp, sc string
		switch {
		case !r.Active:
			sp, sc = ruleState(r.Rule)
		case r.Armed:
			sp, sc = "监控中", green("监控中")
		default:
			sp, sc = "已触发·待复位", yellow("已触发·待复位")
		}
		gap := ""
		if r.Active {
			gap = r.Gap
		}
		fmt.Println(pad(r.ID, r.ID, 12) + pad(sc, sp, 14) + pad(r.Symbol, r.Symbol, 12) + pad(r.Desc, r.Desc, 26) + dim(gap))
	}
}
