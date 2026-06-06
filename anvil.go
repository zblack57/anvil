package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"

	"crypto/tls"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ==================== CYBERSECURITY THEME ====================

var (
	cyan      = lipgloss.Color("#00D4FF")
	green     = lipgloss.Color("#00FF9F")
	red       = lipgloss.Color("#FF4757")
	brightRed = lipgloss.Color("#FF0000")
	orange    = lipgloss.Color("#FFA502")
	purple    = lipgloss.Color("#A55EEA")
	gray      = lipgloss.Color("#888888")
	white     = lipgloss.Color("#FFFFFF")

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(cyan)

	headerBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.BlockBorder()).
			BorderForeground(cyan).
			Padding(0, 1)

	statBoxBase = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(1, 2).
			Width(20)

	liveStyle = lipgloss.NewStyle().
			Foreground(green).
			Bold(true)

	dangerStyle = lipgloss.NewStyle().
			Foreground(red).
			Bold(true)

	successStyle = lipgloss.NewStyle().
			Foreground(green).
			Bold(true)

	subtleStyle = lipgloss.NewStyle().
			Foreground(gray)
)

const (
	defaultURL         = "https://httpbin.org/get"
	defaultRPS         = 10
	defaultConcurrency = 8
	defaultDuration    = 60 * time.Second
	defaultTimeout     = 30 * time.Second
	maxRPS             = 100000
	maxConcurrency     = 10000
)

// ==================== STATS ====================

type Stats struct {
	mu              sync.Mutex
	totalRequests   int64
	successful      int64
	failed          int64
	transientErrors int64
	statusCodes     map[int]int64
	totalLatency    time.Duration
	minLatency      time.Duration
	maxLatency      time.Duration
	startTime       time.Time
}

func (s *Stats) Record(latency time.Duration, status int, err error, isTransient bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalRequests++
	s.totalLatency += latency

	if s.minLatency == 0 || latency < s.minLatency {
		s.minLatency = latency
	}
	if latency > s.maxLatency {
		s.maxLatency = latency
	}

	if err != nil {
		s.failed++
		if isTransient {
			s.transientErrors++
		}
	} else if status >= 200 && status < 300 {
		s.successful++
	} else {
		s.failed++
	}

	if s.statusCodes == nil {
		s.statusCodes = make(map[int]int64)
	}
	s.statusCodes[status]++
}

func (s *Stats) Snapshot() (total, success, failed, transient int64, avgLat, minLat, maxLat time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	total = s.totalRequests
	success = s.successful
	failed = s.failed
	transient = s.transientErrors

	if total > 0 {
		avgLat = s.totalLatency / time.Duration(total)
	}
	minLat = s.minLatency
	maxLat = s.maxLatency
	return
}

// ==================== LOAD TEST ENGINE ====================

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	lower := strings.ToLower(err.Error())
	for _, kw := range []string{"timeout", "connection reset", "eof", "broken pipe", "i/o timeout"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func doRequest(ctx context.Context, client *http.Client, url, method string, stats *Stats, maxRetries int, baseBackoff time.Duration) {
	var lastErr error
	var lastLatency time.Duration

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return
		}

		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			stats.Record(0, 0, err, false)
			return
		}

		req.Header.Set("User-Agent", "A.N.V.I.L/1.0-Cyber")
		req.Header.Set("Accept", "*/*")

		start := time.Now()
		resp, err := client.Do(req)
		lastLatency = time.Since(start)

		if err != nil {
			lastErr = err
			if isTransientError(err) && attempt < maxRetries {
				backoff := baseBackoff * time.Duration(1<<uint(attempt))
				if backoff > 8*time.Second {
					backoff = 8 * time.Second
				}
				jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
				select {
				case <-time.After(backoff + jitter):
					continue
				case <-ctx.Done():
					return
				}
			}
			stats.Record(lastLatency, 0, err, isTransientError(err))
			return
		}

		if method == "GET" {
			done := make(chan struct{})
			go func() {
				io.Copy(io.Discard, resp.Body)
				close(done)
			}()
			select {
			case <-done:
				resp.Body.Close()
			case <-ctx.Done():
				resp.Body.Close()
				stats.Record(lastLatency, resp.StatusCode, ctx.Err(), true)
				return
			}
		} else {
			resp.Body.Close()
		}

		stats.Record(lastLatency, resp.StatusCode, nil, false)
		return
	}

	if lastErr != nil {
		stats.Record(lastLatency, 0, lastErr, isTransientError(lastErr))
	}
}

// ==================== TUI MODEL ====================

type screen int

const (
	screenForm screen = iota
	screenRunning
	screenResults
)

type model struct {
	screen     screen
	inputs     []textinput.Model
	focusIndex int

	// Config
	url         string
	rps         int
	concurrency int
	duration    time.Duration
	method      string
	timeout     time.Duration

	// Runtime
	stats     *Stats
	ctx       context.Context
	cancel    context.CancelFunc
	client    *http.Client
	isRunning bool
	startTime time.Time
	elapsed   time.Duration
	blink     bool // untuk indikator LIVE berkedip

	// Final Results
	finalTotal     int64
	finalSuccess   int64
	finalFailed    int64
	finalTransient int64
	finalAvgLat    time.Duration
	finalMinLat    time.Duration
	finalMaxLat    time.Duration
}

func initialModel() model {
	inputs := make([]textinput.Model, 6)

	inputs[0] = textinput.New()
	inputs[0].Placeholder = "https://target.com"
	inputs[0].Focus()
	inputs[0].Width = 50
	inputs[0].Prompt = "Target URL     : "

	inputs[1] = textinput.New()
	inputs[1].Placeholder = "15"
	inputs[1].Width = 12
	inputs[1].Prompt = "Target RPS     : "

	inputs[2] = textinput.New()
	inputs[2].Placeholder = "8"
	inputs[2].Width = 10
	inputs[2].Prompt = "Concurrency    : "

	inputs[3] = textinput.New()
	inputs[3].Placeholder = "2m"
	inputs[3].Width = 12
	inputs[3].Prompt = "Duration       : "

	inputs[4] = textinput.New()
	inputs[4].Placeholder = "GET / HEAD"
	inputs[4].Width = 12
	inputs[4].Prompt = "Method         : "

	inputs[5] = textinput.New()
	inputs[5].Placeholder = "30s"
	inputs[5].Width = 12
	inputs[5].Prompt = "Timeout        : "

	return model{
		screen: screenForm,
		inputs: inputs,
		stats:  &Stats{},
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.screen == screenRunning {
				if m.cancel != nil {
					m.cancel()
				}
				m.captureFinalStats()
				m.screen = screenResults
				return m, nil
			}
			return m, tea.Quit

		case "s":
			if m.screen == screenRunning {
				if m.cancel != nil {
					m.cancel()
				}
				m.captureFinalStats()
				m.screen = screenResults
				return m, nil
			}

		case "enter":
			if m.screen == screenForm && m.focusIndex == len(m.inputs)-1 {
				m.parseInputs()
				return m, func() tea.Msg { return startTestMsg{} }
			}
			if m.screen == screenForm {
				m.focusIndex = (m.focusIndex + 1) % len(m.inputs)
				m.updateFocus()
			}

		case "tab", "down":
			if m.screen == screenForm {
				m.focusIndex = (m.focusIndex + 1) % len(m.inputs)
				m.updateFocus()
			}
		case "shift+tab", "up":
			if m.screen == screenForm {
				m.focusIndex = (m.focusIndex - 1 + len(m.inputs)) % len(m.inputs)
				m.updateFocus()
			}
		}

	case startTestMsg:
		m.screen = screenRunning
		m.isRunning = true
		m.startTime = time.Now()
		m.ctx, m.cancel = context.WithTimeout(context.Background(), m.duration)
		m.client = newHTTPClient(m.concurrency, m.timeout)
		m.stats = &Stats{startTime: m.startTime}
		return m, tea.Batch(m.tickCmd(), runLoadTestCmd(m.ctx, m.client, m.stats, m.url, m.method, m.rps, m.concurrency))

	case tickMsg:
		if m.screen == screenRunning {
			m.elapsed = time.Since(m.startTime)
			m.blink = !m.blink // toggle untuk efek berkedip
			return m, m.tickCmd()
		}

	case testDoneMsg:
		if m.screen == screenRunning {
			m.captureFinalStats()
			m.screen = screenResults
			m.isRunning = false
			if m.cancel != nil {
				m.cancel()
			}
			return m, nil
		}
	}

	if m.screen == screenForm {
		cmds := make([]tea.Cmd, len(m.inputs))
		for i := range m.inputs {
			m.inputs[i], cmds[i] = m.inputs[i].Update(msg)
		}
		return m, tea.Batch(cmds...)
	}

	return m, nil
}

func (m *model) updateFocus() {
	for i := range m.inputs {
		if i == m.focusIndex {
			m.inputs[i].Focus()
		} else {
			m.inputs[i].Blur()
		}
	}
}

func (m *model) parseInputs() {
	m.url = normalizeURL(m.inputs[0].Value())

	fmt.Sscanf(m.inputs[1].Value(), "%d", &m.rps)
	if m.rps <= 0 {
		m.rps = defaultRPS
	}
	if m.rps > maxRPS {
		m.rps = maxRPS
	}
	fmt.Sscanf(m.inputs[2].Value(), "%d", &m.concurrency)
	if m.concurrency <= 0 {
		m.concurrency = defaultConcurrency
	}
	if m.concurrency > maxConcurrency {
		m.concurrency = maxConcurrency
	}

	durStr := m.inputs[3].Value()
	if durStr == "" {
		durStr = "1m"
	}
	m.duration, _ = time.ParseDuration(durStr)
	if m.duration <= 0 {
		m.duration = defaultDuration
	}

	m.method = strings.ToUpper(strings.TrimSpace(m.inputs[4].Value()))
	if m.method != "HEAD" && m.method != "GET" {
		m.method = "GET"
	}

	toStr := m.inputs[5].Value()
	if toStr == "" {
		toStr = "30s"
	}
	m.timeout, _ = time.ParseDuration(toStr)
	if m.timeout <= 0 {
		m.timeout = defaultTimeout
	}
}

func normalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultURL
	}
	parsed, err := neturl.ParseRequestURI(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return defaultURL
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return defaultURL
	}
	return raw
}

func newHTTPClient(concurrency int, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:          concurrency + 20,
		MaxIdleConnsPerHost:   concurrency,
		IdleConnTimeout:       120 * time.Second,
		DisableKeepAlives:     false,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}

	return &http.Client{Transport: transport, Timeout: timeout}
}

func runLoadTest(ctx context.Context, client *http.Client, stats *Stats, targetURL, method string, rps, concurrency int) {
	jobChan := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobChan {
				if ctx.Err() != nil {
					return
				}
				doRequest(ctx, client, targetURL, method, stats, 2, 800*time.Millisecond)
			}
		}()
	}

	go func() {
		if rps <= 0 {
			rps = 1
		}
		interval := time.Second / time.Duration(rps)
		if interval <= 0 {
			interval = time.Nanosecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				close(jobChan)
				return
			case <-ticker.C:
				select {
				case jobChan <- struct{}{}:
				default:
				}
			}
		}
	}()

	wg.Wait()
}

type tickMsg time.Time
type startTestMsg struct{}
type testDoneMsg struct{}

func runLoadTestCmd(ctx context.Context, client *http.Client, stats *Stats, targetURL, method string, rps, concurrency int) tea.Cmd {
	return func() tea.Msg {
		runLoadTest(ctx, client, stats, targetURL, method, rps, concurrency)
		return testDoneMsg{}
	}
}

func (m model) tickCmd() tea.Cmd {
	return tea.Tick(400*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *model) captureFinalStats() {
	total, success, failed, transient, avg, minL, maxL := m.stats.Snapshot()
	m.finalTotal = total
	m.finalSuccess = success
	m.finalFailed = failed
	m.finalTransient = transient
	m.finalAvgLat = avg
	m.finalMinLat = minL
	m.finalMaxLat = maxL
}

// ==================== ASCII HEADER HACKER STYLE ====================

func asciiHeader() string {
	art := `
========================================
   █████╗ ███╗   ██╗██╗   ██╗██╗██╗     
  ██╔══██╗████╗  ██║██║   ██║██║██║     
  ███████║██╔██╗ ██║██║   ██║██║██║     
  ██╔══██║██║╚██╗██║╚██╗ ██╔╝██║██║     
  ██║  ██║██║ ╚████║ ╚████╔╝ ██║███████╗
  ╚═╝  ╚═╝╚═╝  ╚═══╝  ╚═══╝  ╚═╝╚══════╝ v1.0
=========================================
       github.com/zblack57/anvil`

	return lipgloss.NewStyle().
		Foreground(cyan).
		Render(art)
}

// ==================== VIEW ====================

func (m model) View() string {
	switch m.screen {
	case screenForm:
		return m.viewFormSafe()
	case screenRunning:
		return m.viewRunningSafe()
	case screenResults:
		return m.viewResultsSafe()
	}
	return ""
}

func (m model) viewForm() string {
	var b strings.Builder

	b.WriteString(asciiHeader())
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("  A.N.V.I.L  —  Cyber Load Testing Framework"))
	b.WriteString("\n\n")

	for i, input := range m.inputs {
		style := lipgloss.NewStyle()
		if i == m.focusIndex {
			style = style.Foreground(cyan).Bold(true)
		}
		b.WriteString(style.Render(input.View()))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(subtleStyle.Render("  Tab/↑↓ = Navigate   •   Enter = Start Attack   •   q = Quit"))
	return b.String()
}

func (m model) viewRunning() string {
	total, success, failed, transient, avgLat, minLat, maxLat := m.stats.Snapshot()

	successRate := 0.0
	if total > 0 {
		successRate = float64(success) / float64(total) * 100
	}

	actualRPS := 0.0
	if m.elapsed.Seconds() > 0 {
		actualRPS = float64(total) / m.elapsed.Seconds()
	}

	// === Dynamic Color untuk Error ===
	failedColor := "#00FF9F"
	if failed > 5 {
		failedColor = "#FFA502"
	}
	if failed > 15 {
		failedColor = "#FF4757"
	}
	if failed > 30 {
		failedColor = "#FF0000"
	}

	// === Stat Boxes ===
	box1 := m.cyberStatBox("REQUESTS", fmt.Sprintf("%d", total), "#00D4FF")
	box2 := m.cyberStatBox("SUCCESS", fmt.Sprintf("%.1f%%", successRate), "#00FF9F")
	box3 := m.cyberStatBox("FAILED", fmt.Sprintf("%d", failed), failedColor)
	box4 := m.cyberStatBox("TRANSIENT", fmt.Sprintf("%d", transient), "#FFA502")

	statsRow := lipgloss.JoinHorizontal(lipgloss.Top, box1, box2, box3, box4)

	// === Info Panel ===
	infoContent := fmt.Sprintf(
		"Target URL     : %s\n"+
			"HTTP Method    : %s\n"+
			"Target RPS     : %d     |  Actual: %.1f\n"+
			"Concurrency    : %d\n"+
			"Elapsed Time   : %v",
		m.url, m.method, m.rps, actualRPS, m.concurrency, m.elapsed.Round(time.Second),
	)

	infoPanel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cyan).
		Padding(1, 2).
		Width(58).
		Render(infoContent)

	// === Latency Panel ===
	latencyContent := fmt.Sprintf(
		"Average Latency : %v\n"+
			"Minimum Latency : %v\n"+
			"Maximum Latency : %v",
		avgLat, minLat, maxLat,
	)

	latencyPanel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(purple).
		Padding(1, 2).
		Width(40).
		Render(latencyContent)

	panels := lipgloss.JoinHorizontal(lipgloss.Top, infoPanel, latencyPanel)

	// === LIVE Indicator (Berkedip) ===
	liveIndicator := ""
	if m.blink {
		liveIndicator = liveStyle.Render("● LIVE ATTACK IN PROGRESS")
	} else {
		liveIndicator = subtleStyle.Render("○ LIVE ATTACK IN PROGRESS")
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		asciiHeader(),
		"",
		m.renderHeaderWithStatus(liveIndicator),
		"",
		statsRow,
		"",
		panels,
		"",
		dangerStyle.Render("  Press [s] to stop the attack gracefully"),
	)
}

func (m model) cyberStatBox(title, value, accentColor string) string {
	titleStyled := lipgloss.NewStyle().
		Foreground(lipgloss.Color(accentColor)).
		Bold(true).
		MarginBottom(1).
		Render(title)

	valueStyled := lipgloss.NewStyle().
		Foreground(white).
		Bold(true).
		Width(16).
		Align(lipgloss.Center).
		Render(value)

	box := statBoxBase.
		BorderForeground(lipgloss.Color(accentColor)).
		Render(titleStyled + "\n" + valueStyled)

	return box
}

func (m model) renderHeaderWithStatus(status string) string {
	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(cyan).
		Render("⚒ A.N.V.I.L")

	sub := subtleStyle.Render("Cybersecurity Load Testing Framework")

	left := lipgloss.JoinVertical(lipgloss.Left, title, sub)
	right := lipgloss.PlaceHorizontal(35, lipgloss.Right, status)

	return headerBoxStyle.Render(
		lipgloss.JoinHorizontal(lipgloss.Center, left, right),
	)
}

func (m model) viewResults() string {
	successRate := 0.0
	if m.finalTotal > 0 {
		successRate = float64(m.finalSuccess) / float64(m.finalTotal) * 100
	}

	// Warna berdasarkan hasil
	resultColor := green
	if m.finalFailed > m.finalSuccess/2 {
		resultColor = red
	}

	content := fmt.Sprintf(
		"%s\n\n"+
			"Total Requests   : %d\n"+
			"Successful       : %d (%s)\n"+
			"Failed           : %d\n"+
			"Transient Errors : %d\n\n"+
			"Avg Latency      : %v\n"+
			"Min / Max        : %v / %v",
		lipgloss.NewStyle().Foreground(resultColor).Bold(true).Render("✓ LOAD TEST COMPLETED"),
		m.finalTotal,
		m.finalSuccess,
		fmt.Sprintf("%.1f%%", successRate),
		m.finalFailed,
		m.finalTransient,
		m.finalAvgLat,
		m.finalMinLat,
		m.finalMaxLat,
	)

	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(resultColor).
		Padding(2, 4).
		Align(lipgloss.Center).
		Render(content)

	return lipgloss.JoinVertical(
		lipgloss.Center,
		asciiHeader(),
		"",
		box,
		"",
		subtleStyle.Render("Press [q] to exit"),
	)
}

func safeHeader() string {
	art := `
###################################
    A.N.V.I.L
    Cyber Load Testing Framework
###################################`

	return lipgloss.NewStyle().
		Foreground(cyan).
		Render(art)
}

func (m model) viewFormSafe() string {
	var b strings.Builder

	b.WriteString(asciiHeader())
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("  A.N.V.I.L - Cyber Load Testing Framework"))
	b.WriteString("\n\n")

	for i, input := range m.inputs {
		style := lipgloss.NewStyle()
		if i == m.focusIndex {
			style = style.Foreground(cyan).Bold(true)
		}
		b.WriteString(style.Render(input.View()))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(subtleStyle.Render("  Tab/Up/Down = Navigate   |   Enter = Start Attack   |   q = Quit"))
	return b.String()
}

func (m model) viewRunningSafe() string {
	total, success, failed, transient, avgLat, minLat, maxLat := m.stats.Snapshot()

	successRate := 0.0
	if total > 0 {
		successRate = float64(success) / float64(total) * 100
	}

	actualRPS := 0.0
	if m.elapsed.Seconds() > 0 {
		actualRPS = float64(total) / m.elapsed.Seconds()
	}

	failedColor := "#00FF9F"
	if failed > 5 {
		failedColor = "#FFA502"
	}
	if failed > 15 {
		failedColor = "#FF4757"
	}
	if failed > 30 {
		failedColor = "#FF0000"
	}

	box1 := m.cyberStatBox("REQUESTS", fmt.Sprintf("%d", total), "#00D4FF")
	box2 := m.cyberStatBox("SUCCESS", fmt.Sprintf("%.1f%%", successRate), "#00FF9F")
	box3 := m.cyberStatBox("FAILED", fmt.Sprintf("%d", failed), failedColor)
	box4 := m.cyberStatBox("TRANSIENT", fmt.Sprintf("%d", transient), "#FFA502")
	statsRow := lipgloss.JoinHorizontal(lipgloss.Top, box1, box2, box3, box4)

	infoContent := fmt.Sprintf(
		"Target URL     : %s\n"+
			"HTTP Method    : %s\n"+
			"Target RPS     : %d     |  Actual: %.1f\n"+
			"Concurrency    : %d\n"+
			"Elapsed Time   : %v",
		m.url, m.method, m.rps, actualRPS, m.concurrency, m.elapsed.Round(time.Second),
	)

	infoPanel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cyan).
		Padding(1, 2).
		Width(58).
		Render(infoContent)

	latencyContent := fmt.Sprintf(
		"Average Latency : %v\n"+
			"Minimum Latency : %v\n"+
			"Maximum Latency : %v",
		avgLat, minLat, maxLat,
	)

	latencyPanel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(purple).
		Padding(1, 2).
		Width(40).
		Render(latencyContent)

	panels := lipgloss.JoinHorizontal(lipgloss.Top, infoPanel, latencyPanel)

	liveIndicator := subtleStyle.Render("[    ] ATTACK IN PROGRESS")
	if m.blink {
		liveIndicator = liveStyle.Render("[LIVE] ATTACK IN PROGRESS")
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		asciiHeader(),
		"",
		m.renderHeaderWithStatusSafe(liveIndicator),
		"",
		statsRow,
		"",
		panels,
		"",
		dangerStyle.Render("  Press [s] to stop the attack gracefully"),
	)
}

func (m model) renderHeaderWithStatusSafe(status string) string {
	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(cyan).
		Render("A.N.V.I.L")

	sub := subtleStyle.Render("Cybersecurity Load Testing Framework")

	left := lipgloss.JoinVertical(lipgloss.Left, title, sub)
	right := lipgloss.PlaceHorizontal(35, lipgloss.Right, status)

	return headerBoxStyle.Render(
		lipgloss.JoinHorizontal(lipgloss.Center, left, right),
	)
}

func (m model) viewResultsSafe() string {
	successRate := 0.0
	if m.finalTotal > 0 {
		successRate = float64(m.finalSuccess) / float64(m.finalTotal) * 100
	}

	resultColor := green
	if m.finalFailed > m.finalSuccess/2 {
		resultColor = red
	}

	content := fmt.Sprintf(
		"%s\n\n"+
			"Total Requests   : %d\n"+
			"Successful       : %d (%s)\n"+
			"Failed           : %d\n"+
			"Transient Errors : %d\n\n"+
			"Avg Latency      : %v\n"+
			"Min / Max        : %v / %v",
		lipgloss.NewStyle().Foreground(resultColor).Bold(true).Render("[OK] LOAD TEST COMPLETED"),
		m.finalTotal,
		m.finalSuccess,
		fmt.Sprintf("%.1f%%", successRate),
		m.finalFailed,
		m.finalTransient,
		m.finalAvgLat,
		m.finalMinLat,
		m.finalMaxLat,
	)

	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(resultColor).
		Padding(2, 4).
		Align(lipgloss.Center).
		Render(content)

	return lipgloss.JoinVertical(
		lipgloss.Center,
		asciiHeader(),
		"",
		box,
		"",
		subtleStyle.Render("Press [q] to exit"),
	)
}

func main() {
	rand.Seed(time.Now().UnixNano())

	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running A.N.V.I.L: %v\n", err)
		os.Exit(1)
	}
}
