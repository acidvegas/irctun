package main

import (
	"bufio"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ircAddr   = "irc.supernets.org:6667"
	defChan   = "#superbowl"
	listen    = ":6667"
	defW      = 80
	defH      = 24
	chanListW = 14
	nickListW = 22
	maxMsgs   = 500
)

var words = []string{
	"dark", "cyber", "acid", "hex", "null", "void", "byte", "neo", "max",
	"zed", "fox", "ace", "dex", "arc", "zen", "wolf", "lynx", "hawk",
	"echo", "nova", "ash", "sol", "mint", "jade", "ruby", "axel", "rex",
	"tux", "blaze", "storm", "ghost", "frost", "steel", "chrome", "sigma",
}

func randNick() string {
	return words[rand.Intn(len(words))] + fmt.Sprintf("%d", rand.Intn(900)+100)
}

// ── ANSI ──

const (
	rst     = "\033[0m"
	bold    = "\033[1m"
	clrLine = "\033[2K"
	clrEOL  = "\033[K"
	hideCur = "\033[?25l"
	showCur = "\033[?25h"
	clrScr  = "\033[2J\033[H"

	fgRed     = "\033[31m"
	fgGreen   = "\033[32m"
	fgYellow  = "\033[33m"
	fgBlue    = "\033[34m"
	fgMagenta = "\033[35m"
	fgCyan    = "\033[36m"
	fgWhite   = "\033[37m"
	fgGrey    = "\033[90m"
	fgBlack   = "\033[30m"

	bgBlue    = "\033[44m"
	bgGreen   = "\033[42m"
	bgMagenta = "\033[45m"
)

func pos(r, c int) string { return fmt.Sprintf("\033[%d;%dH", r, c) }

var nickColors = []string{fgRed, fgGreen, fgYellow, fgBlue, fgMagenta, fgCyan}

func nickColor(nick string) string {
	h := 0
	for _, c := range nick {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return nickColors[h%len(nickColors)]
}

func truncVis(s string, maxW int) string {
	var b strings.Builder
	vis := 0
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			b.WriteRune(r)
			continue
		}
		if inEsc {
			b.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if vis >= maxW {
			break
		}
		b.WriteRune(r)
		vis++
	}
	return b.String()
}

func simpleAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// ── Nick sorting ──
// ~ owner > & admin > @ op > % halfop > + voice > regular
// Case-insensitive within each tier

func nickSortKey(display string) string {
	tier := '5'
	for _, ch := range display {
		switch ch {
		case '~':
			tier = '0'
		case '&':
			tier = '1'
		case '@':
			tier = '2'
		case '%':
			tier = '3'
		case '+':
			tier = '4'
		default:
			return string(tier) + strings.ToLower(display)
		}
	}
	return string(tier) + strings.ToLower(display)
}

func sortedNicks(nicks map[string]string) []string {
	list := make([]string, 0, len(nicks))
	for _, d := range nicks {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool {
		return nickSortKey(list[i]) < nickSortKey(list[j])
	})
	return list
}

func nickDisplayColor(n string) string {
	for _, ch := range n {
		switch ch {
		case '~':
			return fgRed + bold
		case '&':
			return fgRed
		case '@':
			return fgGreen
		case '%':
			return fgCyan
		case '+':
			return fgYellow
		default:
			return fgWhite
		}
	}
	return fgWhite
}

// ── Telnet IAC ──

const (
	iacByte = 0xFF
	iacSB   = 0xFA
	iacSE   = 0xF0
	iacWILL = 0xFB
	iacWONT = 0xFC
	iacDO   = 0xFD
	iacDONT = 0xFE
	optNAWS = 0x1F
)

// ── IRC parser ──

type ircMsg struct {
	prefix, command string
	params          []string
}

func (m ircMsg) nick() string {
	if i := strings.IndexByte(m.prefix, '!'); i >= 0 {
		return m.prefix[:i]
	}
	return m.prefix
}

func (m ircMsg) trail() string {
	if len(m.params) > 0 {
		return m.params[len(m.params)-1]
	}
	return ""
}

func parseIRC(line string) ircMsg {
	var m ircMsg
	if len(line) > 0 && line[0] == ':' {
		i := strings.IndexByte(line, ' ')
		if i < 0 {
			return ircMsg{prefix: line[1:]}
		}
		m.prefix = line[1:i]
		line = line[i+1:]
	}
	for line != "" {
		if line[0] == ':' {
			m.params = append(m.params, line[1:])
			break
		}
		i := strings.IndexByte(line, ' ')
		if i < 0 {
			if m.command == "" {
				m.command = line
			} else {
				m.params = append(m.params, line)
			}
			break
		}
		if m.command == "" {
			m.command = line[:i]
		} else {
			m.params = append(m.params, line[:i])
		}
		line = line[i+1:]
	}
	m.command = strings.ToUpper(m.command)
	return m
}

// ── Channel buffer ──

type Channel struct {
	name       string
	topic      string
	mode       string // e.g. "+nst"
	msgs       []string
	nicks      map[string]string
	nickScroll int
	unread     bool
	highlight  bool
}

func newChannel(name string) *Channel {
	return &Channel{name: name, nicks: make(map[string]string)}
}

func (c *Channel) addMsg(line string) {
	c.msgs = append(c.msgs, line)
	if len(c.msgs) > maxMsgs {
		c.msgs = c.msgs[len(c.msgs)-maxMsgs:]
	}
}

// ── clientReader ──

type clientReader struct {
	conn net.Conn
	sess *Session
	buf  []byte
	tmp  [2048]byte
}

func (r *clientReader) ReadLine() (string, error) {
	for {
		r.extractCPR()
		for i, b := range r.buf {
			if b == '\n' {
				line := strings.TrimRight(string(r.buf[:i]), "\r")
				r.buf = r.buf[i+1:]
				return strings.TrimSpace(line), nil
			}
		}
		r.conn.SetReadDeadline(time.Now().Add(300 * time.Second))
		n, err := r.conn.Read(r.tmp[:])
		if err != nil {
			return "", err
		}
		r.ingest(r.tmp[:n])
	}
}

func (r *clientReader) ingest(data []byte) {
	i := 0
	for i < len(data) {
		b := data[i]
		if b == iacByte && i+1 < len(data) {
			consumed := r.handleIAC(data, i)
			if consumed > 0 {
				i += consumed
				continue
			}
		}
		r.buf = append(r.buf, b)
		i++
	}
}

func (r *clientReader) handleIAC(data []byte, i int) int {
	if i+1 >= len(data) {
		return 1
	}
	cmd := data[i+1]
	switch cmd {
	case iacByte:
		r.buf = append(r.buf, 0xFF)
		return 2
	case iacWILL, iacWONT, iacDO, iacDONT:
		if i+2 >= len(data) {
			return 2
		}
		return 3
	case iacSB:
		for j := i + 2; j < len(data)-1; j++ {
			if data[j] == iacByte && data[j+1] == iacSE {
				sub := data[i+2 : j]
				if len(sub) >= 5 && sub[0] == optNAWS {
					w := int(sub[1])<<8 | int(sub[2])
					h := int(sub[3])<<8 | int(sub[4])
					if w > 10 && h > 5 {
						r.sess.resize(w, h)
					}
				}
				return j + 2 - i
			}
		}
		return len(data) - i
	default:
		return 2
	}
}

func (r *clientReader) extractCPR() {
	i := 0
	for i < len(r.buf) {
		if r.buf[i] == 0x1B && i+1 < len(r.buf) && r.buf[i+1] == '[' {
			j := i + 2
			for j < len(r.buf) && ((r.buf[j] >= '0' && r.buf[j] <= '9') || r.buf[j] == ';') {
				j++
			}
			if j < len(r.buf) && r.buf[j] == 'R' {
				nums := string(r.buf[i+2 : j])
				parts := strings.Split(nums, ";")
				if len(parts) == 2 {
					row := simpleAtoi(parts[0])
					col := simpleAtoi(parts[1])
					if row > 5 && col > 10 {
						r.sess.resize(col, row)
					}
				}
				r.buf = append(r.buf[:i], r.buf[j+1:]...)
				continue
			}
		}
		i++
	}
}

// ── Session ──

type Session struct {
	conn    net.Conn
	irc     net.Conn
	nick    string
	w, h    int
	mu      sync.Mutex
	writeMu sync.Mutex
	alive   bool

	channels   []*Channel
	active     int
	showNick   bool
	showChan   bool
	serverCh   *Channel
	history    []string
	historyPos int
}

func newSession(conn net.Conn) *Session {
	srv := newChannel("*status")
	return &Session{
		conn:     conn,
		nick:     randNick(),
		w:        defW,
		h:        defH,
		alive:    true,
		channels: []*Channel{srv},
		active:   0,
		showNick: true,
		showChan: true,
		serverCh: srv,
	}
}

func (s *Session) raw(data string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	s.conn.Write([]byte(data))
}

func (s *Session) ircSend(line string) {
	if s.irc != nil {
		s.irc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		s.irc.Write([]byte(line + "\r\n"))
	}
}

func (s *Session) getChan(name string) *Channel {
	low := strings.ToLower(name)
	for _, c := range s.channels {
		if strings.ToLower(c.name) == low {
			return c
		}
	}
	return nil
}

func (s *Session) getOrMakeChan(name string) *Channel {
	if c := s.getChan(name); c != nil {
		return c
	}
	c := newChannel(name)
	s.channels = append(s.channels, c)
	return c
}

func (s *Session) activeChan() *Channel {
	if s.active < len(s.channels) {
		return s.channels[s.active]
	}
	return s.serverCh
}

func (s *Session) switchTo(name string) bool {
	low := strings.ToLower(name)
	for i, c := range s.channels {
		if strings.ToLower(c.name) == low {
			s.active = i
			c.unread = false
			c.highlight = false
			return true
		}
	}
	return false
}

func (s *Session) switchToIdx(idx int) bool {
	if idx >= 0 && idx < len(s.channels) {
		s.active = idx
		s.channels[idx].unread = false
		s.channels[idx].highlight = false
		return true
	}
	return false
}

func (s *Session) removeChan(name string) {
	low := strings.ToLower(name)
	for i, c := range s.channels {
		if strings.ToLower(c.name) == low {
			s.channels = append(s.channels[:i], s.channels[i+1:]...)
			if s.active >= len(s.channels) {
				s.active = len(s.channels) - 1
			}
			if s.active < 0 {
				s.active = 0
			}
			_ = c
			return
		}
	}
}

func (s *Session) fmtMsg(format string, args ...interface{}) string {
	ts := fgGrey + time.Now().Format("15:04") + rst
	return ts + " " + fmt.Sprintf(format, args...)
}

func (s *Session) addMsgTo(ch *Channel, line string) {
	ch.addMsg(line)
	if ch != s.activeChan() {
		ch.unread = true
	}
}

func (s *Session) chansWithNick(nick string) []*Channel {
	low := strings.ToLower(nick)
	var result []*Channel
	for _, c := range s.channels {
		if _, ok := c.nicks[low]; ok {
			result = append(result, c)
		}
	}
	return result
}

// ── History (call with mu held) ──

func (s *Session) addHistory(line string) {
	for i, h := range s.history {
		if h == line {
			s.history = append(s.history[:i], s.history[i+1:]...)
			break
		}
	}
	s.history = append(s.history, line)
	if len(s.history) > 100 {
		s.history = s.history[len(s.history)-100:]
	}
	s.historyPos = len(s.history)
}

// parseArrows strips arrow key escape sequences from a line and returns
// the cleaned text plus the count of up/down presses.
func parseArrows(line string) (string, int, int) {
	var b strings.Builder
	raw := []byte(line)
	ups, downs := 0, 0
	i := 0
	for i < len(raw) {
		if raw[i] == 0x1B && i+2 < len(raw) && raw[i+1] == '[' {
			switch raw[i+2] {
			case 'A':
				ups++
				i += 3
				continue
			case 'B':
				downs++
				i += 3
				continue
			default:
				// skip other CSI sequences (e.g. \033[C, \033[D cursor left/right)
				i += 3
				continue
			}
		}
		b.WriteByte(raw[i])
		i++
	}
	return b.String(), ups, downs
}

// ── Layout (call with mu held) ──

func (s *Session) clW() int {
	if !s.showChan || s.w < 50 {
		return 0
	}
	return chanListW
}

func (s *Session) nlW() int {
	if !s.showNick || s.w < 60 {
		return 0
	}
	return nickListW
}

func (s *Session) mainH() int {
	h := s.h - 3 // row 1 top bar, row H-1 status, row H input
	if h < 1 {
		h = 1
	}
	return h
}

func (s *Session) querySize() {
	s.raw("\033[s\033[999;999H\033[6n\033[u")
}

func (s *Session) resize(w, h int) {
	s.mu.Lock()
	if w == s.w && h == s.h {
		s.mu.Unlock()
		return
	}
	s.w = w
	s.h = h
	s.mu.Unlock()
	s.raw("\033[r" + clrScr) // Reset scroll region then clear
	s.draw()
}

// ── Drawing ──
// Row 1:        Top bar (channel + mode + topic)
// Row 2..H-2:   [chanlist │ chat │ nicklist]  (mainH rows)
// Row H-1:      Status bar
// Row H:        nick » input (single line)

func (s *Session) draw() {
	s.mu.Lock()

	w := s.w
	h := s.h
	mH := s.mainH()
	clW := s.clW()
	ac := s.activeChan()
	nlW := s.nlW()
	// Hide nicklist on status and PM windows (only show for channels)
	if !strings.HasPrefix(ac.name, "#") {
		nlW = 0
	}
	nick := s.nick

	// Copy state under lock
	chanName := ac.name
	chanMode := ac.mode
	chanTopic := ac.topic
	nickCount := len(ac.nicks)
	nickScroll := ac.nickScroll
	activeIdx := s.active

	msgs := make([]string, len(ac.msgs))
	copy(msgs, ac.msgs)

	type ci struct {
		name      string
		unread    bool
		highlight bool
	}
	chans := make([]ci, len(s.channels))
	for i, c := range s.channels {
		n := c.name
		if i == 0 {
			n = "status"
		}
		chans[i] = ci{n, c.unread, c.highlight}
	}

	var allNicks []string
	if nlW > 0 {
		allNicks = sortedNicks(ac.nicks)
	}

	s.mu.Unlock()

	// Column math
	seps := 0
	if clW > 0 {
		seps++
	}
	if nlW > 0 {
		seps++
	}
	cw := w - clW - nlW - seps
	if cw < 5 {
		cw = 5
	}
	chatCol := 1
	if clW > 0 {
		chatCol = clW + 2
	}

	// Message window
	msgStart := 0
	if len(msgs) > mH {
		msgStart = len(msgs) - mH
	}

	// Nick scroll logic
	dataRows := mH - 1 // minus header row
	needScroll := len(allNicks) > dataRows
	showUp := needScroll && nickScroll > 0
	avail := dataRows
	if showUp {
		avail--
	}
	showDown := needScroll && (nickScroll+avail) < len(allNicks)
	if showDown {
		avail--
	}

	// Build frame
	var f strings.Builder
	f.Grow(16384)
	f.WriteString("\033[r")    // Reset scroll region for full-screen drawing
	f.WriteString("\033[?7l") // Disable autowrap (prevents input wrapping to next line)
	f.WriteString(hideCur)

	// ── Row 1: Top bar ──
	topText := " " + chanName
	if chanMode != "" {
		topText += " [" + chanMode + "]"
	}
	if chanTopic != "" {
		topText += " │ " + chanTopic
	}
	runes := []rune(topText)
	if len(runes) > w {
		runes = runes[:w]
	}
	f.WriteString(pos(1, 1) + bgBlue + fgWhite + bold)
	f.WriteString(string(runes))
	if pad := w - len(runes); pad > 0 {
		f.WriteString(strings.Repeat(" ", pad))
	}
	f.WriteString(rst)

	// ── Main area: rows 2 .. mH+1 ──
	for i := 0; i < mH; i++ {
		row := i + 2
		f.WriteString(pos(row, 1) + clrLine)

		// Channel list
		if clW > 0 {
			if i < len(chans) {
				label := chans[i].name
				tag := fmt.Sprintf("%d %s", i, label)
				if len(tag) > clW-1 {
					tag = tag[:clW-1]
				}
				if i == activeIdx {
					f.WriteString(bold + fgWhite + " " + tag + rst)
			} else if chans[i].highlight {
				f.WriteString(fgYellow + bold + " " + tag + rst)
				} else if chans[i].unread {
					f.WriteString(fgCyan + " " + tag + rst)
				} else {
					f.WriteString(fgGrey + " " + tag + rst)
				}
			}
			f.WriteString(pos(row, clW+1) + fgGrey + "│" + rst)
		}

		// Chat message
		f.WriteString(pos(row, chatCol))
		if msgStart+i < len(msgs) {
			f.WriteString(truncVis(msgs[msgStart+i], cw))
		}

		// Nick list
		if nlW > 0 {
			nickSep := chatCol + cw
			f.WriteString(pos(row, nickSep) + fgGrey + "│" + rst)
			f.WriteString(pos(row, nickSep+1))

			if i == 0 {
				// Header
				f.WriteString(fgCyan + bold + fmt.Sprintf(" %d nicks", nickCount) + rst)
			} else {
				di := i - 1 // data row index
				if di == 0 && showUp {
					f.WriteString(fgGrey + " ▲ /nup" + rst)
				} else if di == dataRows-1 && showDown {
					f.WriteString(fgGrey + " ▼ /nd" + rst)
				} else {
					adj := di
					if showUp {
						adj = di - 1
					}
					ni := nickScroll + adj
					if ni >= 0 && ni < len(allNicks) {
						n := allNicks[ni]
						display := n
						if len(display) > nlW-1 {
							display = display[:nlW-1]
						}
						col := nickDisplayColor(n)
						f.WriteString(col + " " + display + rst)
					}
				}
			}
			f.WriteString(clrEOL)
		}
	}

	// ── Status bar (row H-1) ──
	statRow := h - 1
	modeTag := ""
	if chanMode != "" {
		modeTag = chanMode
	}
	statText := fmt.Sprintf(" %s │ %s │ %dx%d ", chanName, modeTag, w, h)
	sr := []rune(statText)
	if pad := w - len(sr); pad > 0 {
		statText += strings.Repeat(" ", pad)
	} else if pad := w - len(sr); pad < 0 {
		statText = string(sr[:w])
	}
	f.WriteString(pos(statRow, 1) + bgGreen + fgBlack + bold + statText + rst)

	// ── Input (row H) — single line, cursor on same line ──
	inRow := h
	promptNick := nick
	if len(promptNick) > 15 {
		promptNick = promptNick[:15]
	}
	prompt := fgGreen + bold + promptNick + rst + " » "
	f.WriteString(pos(inRow, 1) + clrLine + prompt)

	// Set scroll region to rows 1..H-1 so Enter on row H can't scroll the layout
	if h > 2 {
		f.WriteString(fmt.Sprintf("\033[1;%dr", h-1))
	}

	// Position cursor right after the prompt on the input line
	promptVis := len([]rune(promptNick)) + 3 // "nick » "
	f.WriteString(pos(inRow, promptVis+1) + showCur)

	s.raw(f.String())
}

// ── Input handling ──

func (s *Session) handleInput(text string) {
	if !strings.HasPrefix(text, "/") {
		s.mu.Lock()
		ac := s.activeChan()
		name := ac.name
		s.mu.Unlock()

		if name == "*status" {
			s.mu.Lock()
			s.serverCh.addMsg(s.fmtMsg(fgGrey + "Cannot send to status. Use /join #channel" + rst))
			s.mu.Unlock()
			s.draw()
			return
		}

		s.ircSend(fmt.Sprintf("PRIVMSG %s :%s", name, text))
		col := nickColor(s.nick)
		s.mu.Lock()
		ac.addMsg(s.fmtMsg(col+"<%s>"+rst+" %s", s.nick, text))
		s.mu.Unlock()
		s.draw()
		return
	}

	parts := strings.SplitN(text, " ", 2)
	cmd := strings.ToLower(parts[0])
	arg := ""
	if len(parts) > 1 {
		arg = parts[1]
	}

	switch cmd {
	case "/quit", "/exit":
		s.ircSend("QUIT :Leaving")
		s.mu.Lock()
		s.activeChan().addMsg(s.fmtMsg(fgGrey + "Goodbye." + rst))
		s.mu.Unlock()
		s.draw()
		time.Sleep(300 * time.Millisecond)
		s.alive = false
		s.conn.Close()

	case "/join", "/j":
		if arg == "" {
			return
		}
		ch := strings.Fields(arg)[0]
		if !strings.HasPrefix(ch, "#") {
			ch = "#" + ch
		}
		s.ircSend("JOIN " + ch)

	case "/part", "/leave":
		s.mu.Lock()
		target := s.activeChan().name
		s.mu.Unlock()
		if arg != "" {
			target = strings.Fields(arg)[0]
		}
		if target == "*status" {
			return
		}
		if strings.HasPrefix(target, "#") {
			s.ircSend("PART " + target)
		}
		s.mu.Lock()
		s.removeChan(target)
		s.mu.Unlock()
		s.raw(clrScr)
		s.draw()

	case "/sw", "/switch", "/w":
		if arg == "" {
			return
		}
		s.mu.Lock()
		if arg[0] >= '0' && arg[0] <= '9' {
			s.switchToIdx(simpleAtoi(arg))
		} else {
			s.switchTo(arg)
		}
		s.mu.Unlock()
		s.raw(clrScr)
		s.draw()

	case "/nick":
		if arg != "" {
			s.ircSend("NICK " + arg)
		}

	case "/me":
		if arg == "" {
			return
		}
		s.mu.Lock()
		ac := s.activeChan()
		name := ac.name
		s.mu.Unlock()
		if name == "*status" {
			return
		}
		s.ircSend(fmt.Sprintf("PRIVMSG %s :\x01ACTION %s\x01", name, arg))
		s.mu.Lock()
		ac.addMsg(s.fmtMsg(fgMagenta+"* %s %s"+rst, s.nick, arg))
		s.mu.Unlock()
		s.draw()

	case "/msg":
		p := strings.SplitN(arg, " ", 2)
		if len(p) >= 1 && p[0] != "" {
			target := p[0]
			s.mu.Lock()
			pm := s.getOrMakeChan(target)
			s.mu.Unlock()
			if len(p) == 2 && p[1] != "" {
				s.ircSend(fmt.Sprintf("PRIVMSG %s :%s", target, p[1]))
				col := nickColor(s.nick)
				s.mu.Lock()
				pm.addMsg(s.fmtMsg(col+"<%s>"+rst+" %s", s.nick, p[1]))
				s.mu.Unlock()
			}
			s.mu.Lock()
			s.switchTo(target)
			s.mu.Unlock()
			s.raw(clrScr)
			s.draw()
		}

	case "/query", "/q":
		if arg == "" {
			return
		}
		target := strings.Fields(arg)[0]
		s.mu.Lock()
		s.getOrMakeChan(target)
		s.switchTo(target)
		s.mu.Unlock()
		s.raw(clrScr)
		s.draw()

	case "/close":
		s.mu.Lock()
		name := s.activeChan().name
		s.mu.Unlock()
		if name == "*status" {
			return
		}
		if strings.HasPrefix(name, "#") {
			s.ircSend("PART " + name)
		}
		s.mu.Lock()
		s.removeChan(name)
		s.mu.Unlock()
		s.raw(clrScr)
		s.draw()

	case "/topic":
		s.mu.Lock()
		name := s.activeChan().name
		s.mu.Unlock()
		if name != "*status" {
			if arg != "" {
				s.ircSend(fmt.Sprintf("TOPIC %s :%s", name, arg))
			} else {
				s.ircSend("TOPIC " + name)
			}
		}

	case "/nicklist", "/nl":
		s.mu.Lock()
		s.showNick = !s.showNick
		vis := "shown"
		if !s.showNick {
			vis = "hidden"
		}
		s.activeChan().addMsg(s.fmtMsg(fgGrey+"Nicklist "+vis+" [/nl]"+rst))
		s.mu.Unlock()
		s.raw(clrScr)
		s.draw()

	case "/chanlist", "/cl":
		s.mu.Lock()
		s.showChan = !s.showChan
		vis := "shown"
		if !s.showChan {
			vis = "hidden"
		}
		s.activeChan().addMsg(s.fmtMsg(fgGrey+"Channel list "+vis+" [/cl]"+rst))
		s.mu.Unlock()
		s.raw(clrScr)
		s.draw()

	case "/nup":
		n := 5
		if arg != "" {
			if v := simpleAtoi(arg); v > 0 {
				n = v
			}
		}
		s.mu.Lock()
		ac := s.activeChan()
		ac.nickScroll -= n
		if ac.nickScroll < 0 {
			ac.nickScroll = 0
		}
		s.mu.Unlock()
		s.draw()

	case "/ndown", "/nd":
		n := 5
		if arg != "" {
			if v := simpleAtoi(arg); v > 0 {
				n = v
			}
		}
		s.mu.Lock()
		ac := s.activeChan()
		total := len(ac.nicks)
		maxScroll := total - s.mainH() + 2
		if maxScroll < 0 {
			maxScroll = 0
		}
		ac.nickScroll += n
		if ac.nickScroll > maxScroll {
			ac.nickScroll = maxScroll
		}
		s.mu.Unlock()
		s.draw()

	case "/redraw", "/rd":
		s.querySize()
		s.raw(clrScr)
		s.draw()

	case "/resize":
		s.querySize()
		s.mu.Lock()
		s.activeChan().addMsg(s.fmtMsg(fgGrey + "Querying terminal size..." + rst))
		s.mu.Unlock()
		s.draw()

	case "/help":
		help := []string{
			fgCyan + bold + "── Commands ──" + rst,
			fgGreen + " /join #channel  " + rst + " Join a channel",
			fgGreen + " /part [#chan]    " + rst + " Leave channel/close PM",
			fgGreen + " /sw <N|#chan>    " + rst + " Switch window",
			fgGreen + " /nick <name>    " + rst + " Change nick",
			fgGreen + " /me <action>    " + rst + " Action message",
			fgGreen + " /msg <to> [txt] " + rst + " Open PM (optional msg)",
			fgGreen + " /query <nick>   " + rst + " Open PM window",
			fgGreen + " /close          " + rst + " Close current window",
			fgGreen + " /topic [text]   " + rst + " View/set topic",
			fgCyan + bold + "── Panels ──" + rst,
			fgGreen + " /nl             " + rst + " Toggle nicklist",
			fgGreen + " /cl             " + rst + " Toggle channel list",
			fgGreen + " /nup [N]        " + rst + " Scroll nicks up",
			fgGreen + " /nd [N]         " + rst + " Scroll nicks down",
			fgCyan + bold + "── Other ──" + rst,
			fgGreen + " /rd             " + rst + " Redraw screen",
			fgGreen + " /resize         " + rst + " Re-detect term size",
			fgGreen + " /quit           " + rst + " Disconnect",
			"",
			fgGrey + " ↑/↓ + Enter = command history" + rst,
		}
		s.mu.Lock()
		ac := s.activeChan()
		for _, l := range help {
			ac.addMsg(s.fmtMsg(l))
		}
		s.mu.Unlock()
		s.draw()

	default:
		s.mu.Lock()
		s.activeChan().addMsg(s.fmtMsg(fgGrey + "Unknown command. Type /help" + rst))
		s.mu.Unlock()
		s.draw()
	}
}

// ── Main session loop ──

func (s *Session) run() {
	defer s.conn.Close()

	cr := &clientReader{conn: s.conn, sess: s}

	// Telnet NAWS negotiation
	s.conn.Write([]byte{iacByte, iacDO, optNAWS})

	// ANSI cursor position report
	s.querySize()

	// Wait for NAWS or CPR response (500ms — telnet usually responds instantly)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.conn.SetReadDeadline(deadline)
		n, err := s.conn.Read(cr.tmp[:])
		if err != nil {
			break
		}
		cr.ingest(cr.tmp[:n])
		cr.extractCPR()
		s.mu.Lock()
		gotSize := s.w != defW || s.h != defH
		s.mu.Unlock()
		if gotSize {
			break
		}
	}
	s.conn.SetReadDeadline(time.Time{})

	// Check if we got the real terminal size
	s.mu.Lock()
	gotSize := s.w != defW || s.h != defH
	s.mu.Unlock()

	if !gotSize {
		// nc users: CPR response is stuck in line buffer until Enter.
		// Show a prompt and wait for Enter to flush the CPR response.
		s.raw(clrScr +
			pos(1, 1) + fgCyan + bold + "  IRC Tunnel" + rst + "\r\n" +
			fgGrey + "  ─────────────────────────────" + rst + "\r\n\r\n" +
			fgGrey + "  Press " + fgWhite + bold + "Enter" + rst + fgGrey + " to begin..." + rst + "\r\n")
		s.querySize() // Queue another CPR query — it'll be flushed with Enter

		_, err := cr.ReadLine()
		if err != nil {
			return
		}
		// extractCPR inside ReadLine should have detected the size by now
	}

	// Initial draw with (hopefully) correct size
	s.raw(clrScr)
	s.mu.Lock()
	s.serverCh.addMsg(s.fmtMsg(fgGrey+"Connecting to "+fgWhite+bold+ircAddr+rst+fgGrey+" as "+fgGreen+s.nick+rst+fgGrey+"..."+rst))
	s.mu.Unlock()
	s.draw()

	// Connect to IRC
	irc, err := net.DialTimeout("tcp", ircAddr, 10*time.Second)
	if err != nil {
		s.mu.Lock()
		s.serverCh.addMsg(s.fmtMsg(fgRed+bold+"Connection failed: "+rst+"%s", err))
		s.mu.Unlock()
		s.draw()
		time.Sleep(3 * time.Second)
		return
	}
	s.irc = irc
	defer irc.Close()

	s.ircSend("NICK " + s.nick)
	s.ircSend("USER tunnel 0 * :Tunnel User")

	done := make(chan struct{})

	// ── IRC reader ──
	go func() {
		defer close(done)
		sc := bufio.NewScanner(irc)
		sc.Buffer(make([]byte, 0, 4096), 4096)
		joined := false

		for sc.Scan() {
			if !s.alive {
				return
			}
			rawLine := sc.Text()
			m := parseIRC(rawLine)

			// Route numeric server replies to status window
			isNum := len(m.command) >= 3 && m.command[0] >= '0' && m.command[0] <= '9'
			if isNum && m.command != "353" && m.command != "366" {
				s.mu.Lock()
				display := m.trail()
				if display == "" {
					display = strings.Join(m.params, " ")
				}
				s.addMsgTo(s.serverCh, s.fmtMsg(fgGrey+"["+m.command+"]"+rst+" %s", display))
				s.mu.Unlock()
			}

			switch m.command {
			case "PING":
				s.ircSend("PONG :" + m.trail())

			case "001":
				if !joined {
					joined = true
					s.ircSend("JOIN " + defChan)
					s.mu.Lock()
					s.serverCh.addMsg(s.fmtMsg(fgGreen + bold + "Connected! Type /help for commands" + rst))
					s.mu.Unlock()
					s.draw()
				}

			case "324": // RPL_CHANNELMODEIS
				if len(m.params) >= 3 {
					chName := m.params[1]
					mode := strings.Join(m.params[2:], " ")
					s.mu.Lock()
					if c := s.getChan(chName); c != nil {
						c.mode = mode
					}
					s.mu.Unlock()
					s.draw()
				}

			case "332": // RPL_TOPIC
				if len(m.params) >= 2 {
					chName := m.params[1]
					topic := m.trail()
					s.mu.Lock()
					if c := s.getChan(chName); c != nil {
						c.topic = topic
					}
					s.mu.Unlock()
					s.draw()
				}

			case "353": // RPL_NAMREPLY (silent — no status dump)
				var chName string
				for _, p := range m.params {
					if strings.HasPrefix(p, "#") {
						chName = p
						break
					}
				}
				if chName == "" {
					continue
				}
				names := strings.Fields(m.trail())
				s.mu.Lock()
				if c := s.getChan(chName); c != nil {
					for _, n := range names {
						clean := strings.TrimLeft(n, "~&@%+")
						c.nicks[strings.ToLower(clean)] = n
					}
				}
				s.mu.Unlock()

			case "366": // RPL_ENDOFNAMES (silent)
				s.draw()

			case "433": // ERR_NICKNAMEINUSE
				s.mu.Lock()
				s.nick = randNick()
				s.mu.Unlock()
				s.ircSend("NICK " + s.nick)
				s.mu.Lock()
				s.serverCh.addMsg(s.fmtMsg(fgGrey+"Nick taken, trying "+fgGreen+s.nick+rst))
				s.mu.Unlock()
				s.draw()

			case "JOIN":
				who := m.nick()
				chName := m.trail()
				if chName == "" && len(m.params) > 0 {
					chName = m.params[0]
				}
				if strings.EqualFold(who, s.nick) {
					s.mu.Lock()
					c := s.getOrMakeChan(chName)
					c.nicks = make(map[string]string)
					c.addMsg(s.fmtMsg(fgGrey+"Joined "+fgCyan+bold+chName+rst))
					c.addMsg(s.fmtMsg(fgGrey+"Type to chat │ /help for commands"+rst))
					s.switchTo(chName)
					s.mu.Unlock()
					// Request channel mode
					s.ircSend("MODE " + chName)
					s.raw(clrScr)
					s.draw()
				} else {
					s.mu.Lock()
					if c := s.getChan(chName); c != nil {
						c.nicks[strings.ToLower(who)] = who
						s.addMsgTo(c, s.fmtMsg(fgGrey+"→ %s joined"+rst, who))
					}
					s.mu.Unlock()
					s.draw()
				}

			case "PART":
				who := m.nick()
				chName := ""
				if len(m.params) > 0 {
					chName = m.params[0]
				}
				if strings.EqualFold(who, s.nick) {
					s.mu.Lock()
					s.removeChan(chName)
					s.mu.Unlock()
					s.raw(clrScr)
					s.draw()
				} else {
					s.mu.Lock()
					if c := s.getChan(chName); c != nil {
						delete(c.nicks, strings.ToLower(who))
						s.addMsgTo(c, s.fmtMsg(fgGrey+"← %s left"+rst, who))
					}
					s.mu.Unlock()
					s.draw()
				}

			case "QUIT":
				who := m.nick()
				reason := m.trail()
				s.mu.Lock()
				for _, c := range s.chansWithNick(who) {
					delete(c.nicks, strings.ToLower(who))
					if reason != "" {
						s.addMsgTo(c, s.fmtMsg(fgGrey+"← %s quit (%s)"+rst, who, reason))
					} else {
						s.addMsgTo(c, s.fmtMsg(fgGrey+"← %s quit"+rst, who))
					}
				}
				s.mu.Unlock()
				s.draw()

			case "PRIVMSG":
				sender := m.nick()
				if len(m.params) < 2 {
					continue
				}
				target := m.params[0]
				msg := m.trail()

				isAction := strings.HasPrefix(msg, "\x01ACTION ") && strings.HasSuffix(msg, "\x01")
				if isAction {
					msg = msg[8 : len(msg)-1]
				}

				s.mu.Lock()
				if strings.HasPrefix(target, "#") {
					c := s.getChan(target)
					if c == nil {
						s.mu.Unlock()
						continue
					}
					if isAction {
						s.addMsgTo(c, s.fmtMsg(fgMagenta+"* %s %s"+rst, sender, msg))
					} else if !strings.EqualFold(sender, s.nick) {
						col := nickColor(sender)
						s.addMsgTo(c, s.fmtMsg(col+"<%s>"+rst+" %s", sender, msg))
						if strings.Contains(strings.ToLower(msg), strings.ToLower(s.nick)) && c != s.activeChan() {
							c.highlight = true
						}
					}
				} else if strings.EqualFold(target, s.nick) {
					// Incoming PM — open/find PM window for sender
					pm := s.getOrMakeChan(sender)
					if isAction {
						s.addMsgTo(pm, s.fmtMsg(fgMagenta+"* %s %s"+rst, sender, msg))
					} else {
						col := nickColor(sender)
						s.addMsgTo(pm, s.fmtMsg(col+"<%s>"+rst+" %s", sender, msg))
					}
					if pm != s.activeChan() {
						pm.highlight = true
					}
				}
				s.mu.Unlock()
				s.draw()

			case "NOTICE":
				sender := m.nick()
				s.mu.Lock()
				// Server notices (no ! in prefix) → status, user notices → active
				if !strings.Contains(m.prefix, "!") {
					s.addMsgTo(s.serverCh, s.fmtMsg(fgYellow+"-%s-"+rst+" %s", sender, m.trail()))
				} else {
					s.activeChan().addMsg(s.fmtMsg(fgYellow+"-%s-"+rst+" %s", sender, m.trail()))
				}
				s.mu.Unlock()
				s.draw()

			case "NICK":
				who := m.nick()
				newN := m.trail()
				if newN == "" && len(m.params) > 0 {
					newN = m.params[0]
				}
				if strings.EqualFold(who, s.nick) {
					s.mu.Lock()
					s.nick = newN
					s.activeChan().addMsg(s.fmtMsg(fgGrey+"You are now "+fgGreen+bold+newN+rst))
					s.mu.Unlock()
				} else {
					s.mu.Lock()
					for _, c := range s.chansWithNick(who) {
						old := strings.ToLower(who)
						pfx := ""
						if d, ok := c.nicks[old]; ok {
							for _, ch := range d {
								if ch == '~' || ch == '&' || ch == '@' || ch == '%' || ch == '+' {
									pfx += string(ch)
								} else {
									break
								}
							}
							delete(c.nicks, old)
						}
						c.nicks[strings.ToLower(newN)] = pfx + newN
						s.addMsgTo(c, s.fmtMsg(fgGrey+"%s → %s"+rst, who, newN))
					}
					s.mu.Unlock()
				}
				s.draw()

			case "KICK":
				if len(m.params) < 2 {
					continue
				}
				chName := m.params[0]
				kicked := m.params[1]
				reason := m.trail()
				if strings.EqualFold(kicked, s.nick) {
					s.mu.Lock()
					if c := s.getChan(chName); c != nil {
						c.addMsg(s.fmtMsg(fgRed+bold+"Kicked! (%s) Rejoining..."+rst, reason))
					}
					s.mu.Unlock()
					s.ircSend("JOIN " + chName)
				} else {
					s.mu.Lock()
					if c := s.getChan(chName); c != nil {
						delete(c.nicks, strings.ToLower(kicked))
						s.addMsgTo(c, s.fmtMsg(fgGrey+"← %s kicked (%s)"+rst, kicked, reason))
					}
					s.mu.Unlock()
				}
				s.draw()

			case "MODE":
				if len(m.params) > 0 && strings.HasPrefix(m.params[0], "#") {
					chName := m.params[0]
					// Re-request mode and names to stay in sync
					s.ircSend("MODE " + chName)
					s.mu.Lock()
					if c := s.getChan(chName); c != nil {
						c.nicks = make(map[string]string)
					}
					s.mu.Unlock()
					s.ircSend("NAMES " + chName)
				}

			default:
				// Unhandled non-numeric commands → status window
				if !isNum {
					s.mu.Lock()
					s.addMsgTo(s.serverCh, s.fmtMsg(fgGrey+m.command+" "+strings.Join(m.params, " ")+rst))
					s.mu.Unlock()
				}
				// Redraw if viewing status
				s.mu.Lock()
				onStatus := s.activeChan() == s.serverCh
				s.mu.Unlock()
				if onStatus {
					s.draw()
				}
			}
		}
	}()

	// ── Client reader ──
	for {
		line, err := cr.ReadLine()
		if err != nil || !s.alive {
			break
		}

		// Immediately clear the input line to remove terminal echo artifacts
		s.mu.Lock()
		inRow := s.h
		s.mu.Unlock()
		s.raw(pos(inRow, 1) + clrLine)

		cleaned, ups, downs := parseArrows(line)
		cleaned = strings.TrimSpace(cleaned)

		if cleaned == "" && (ups > 0 || downs > 0) {
			// Pure arrow key input — history navigation
			s.mu.Lock()
			if len(s.history) == 0 {
				s.mu.Unlock()
				s.draw()
				continue
			}
			delta := ups - downs // positive = go back in history
			s.historyPos -= delta
			if s.historyPos < 0 {
				s.historyPos = 0
			}
			if s.historyPos >= len(s.history) {
				s.historyPos = len(s.history) - 1
			}
			recalled := s.history[s.historyPos]
			s.addHistory(recalled) // moves to end, resets pos
			nick := s.nick
			s.mu.Unlock()

			// Flash the recalled command on the input line so user sees what ran
			promptNick := nick
			if len(promptNick) > 15 {
				promptNick = promptNick[:15]
			}
			s.raw(pos(inRow, 1) + clrLine +
				fgGreen + bold + promptNick + rst + " » " +
				fgYellow + recalled + rst)

			s.handleInput(recalled)
			continue
		}

		if cleaned == "" {
			s.draw()
			continue
		}

		// Normal input — add to history and execute
		s.mu.Lock()
		s.addHistory(cleaned)
		s.mu.Unlock()
		s.handleInput(cleaned)
	}

	s.alive = false
	s.ircSend("QUIT :Client disconnected")
	s.irc.Close()
	<-done
}

func main() {
	rand.Seed(time.Now().UnixNano())
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Printf("Failed to listen: %s\n", err)
		return
	}
	fmt.Printf("Tunnel listening on %s\n", listen)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go newSession(conn).run()
	}
}
