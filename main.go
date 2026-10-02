package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cinar/indicator"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

// ==========================================
// 1. STRUKTUR DATA & KONFIGURASI
// ==========================================

type Config struct {
	ApiKey     string    `json:"-"`
	SecretKey  string    `json:"-"`
	Passphrase string    `json:"-"`
	MarginUSDT float64   `json:"margin"`
	Leverage   int       `json:"leverage"`
	Coins      [5]string `json:"coins"`
	Mode       string    `json:"mode"`
	IsRunning  bool      `json:"isRunning"`
	Timeframe  string    `json:"timeframe"`

	// TakeProfitPct & StopLossPct dalam PERSEN (0.8 = 0.8%).
	// Risk-Reward = StopLossPct : TakeProfitPct. Untuk 1:2 yang benar,
	// StopLossPct harus LEBIH KECIL dari TakeProfitPct.
	TakeProfitPct float64 `json:"takeProfitPct"`
	StopLossPct   float64 `json:"stopLossPct"`

	// Metode penempatan TP/SL:
	//   "percent" -> jarak persen tetap (TakeProfitPct/StopLossPct).
	//   "atr"     -> SL = ATR x AtrSlMult (dinamis mengikuti volatilitas),
	//                TP = 2 x jarak SL (rasio risiko:hadiah selalu 1:2).
	//                Saat data ATR belum cukup, fallback ke persen.
	TpSlMode  string  `json:"tpSlMode"`
	AtrPeriod int     `json:"atrPeriod"`
	AtrSlMult float64 `json:"atrSlMult"`

	// --- F1: Position Sizing -------------------------------------------
	// Mengganti margin tetap (MarginUSDT) saat aktif.
	//   PosSizingMode "pct"   -> margin = Saldo * PosSizingValue/100
	//   PosSizingMode "fixed" -> margin = PosSizingValue (USDT)
	// Saat OFF, margin per trade = MarginUSDT (perilaku lama).
	PosSizingEnabled bool    `json:"posSizingEnabled"`
	PosSizingMode    string  `json:"posSizingMode"` // "pct" | "fixed"
	PosSizingValue   float64 `json:"posSizingValue"`

	// --- F2: Daily Loss Limit ------------------------------------------
	// Shutdown otomatis + tutup semua posisi bila loss harian (realized +
	// unrealized, sejak 00:00 UTC) mencapai batas. Reset tiap 00:00 UTC.
	//   LossLimitMode "pct"  -> batas dihitung dari basis (lihat di bawah)
	//   LossLimitMode "fixed"-> batas = LossLimitValue USDT
	// Basis mode "pct": bila F1 aktif -> nilai Position Sizing (hasil F1),
	// bila F1 OFF -> saldo akun saat ini.
	LossLimitEnabled bool    `json:"lossLimitEnabled"`
	LossLimitMode    string  `json:"lossLimitMode"`  // "pct" | "fixed"
	LossLimitValue   float64 `json:"lossLimitValue"` // default 5 (% atau USDT)

	// --- F3: Time Filter ----------------------------------------------
	// Bot hanya membuka posisi BARU di dalam jendela sesi (UTC). Posisi yang
	// sudah terbuka tetap dikelola (TP/SL, trailing) di luar jendela.
	// TimeFilterMode: "24/7" | "asian" | "london" | "newyork" | "overlap" | "custom"
	TimeFilterMode  string `json:"timeFilterMode"`
	CustomStartHour int    `json:"customStartHour"` // UTC, 0-23 (dipakai mode "custom")
	CustomEndHour   int    `json:"customEndHour"`   // UTC, 0-23 (dipakai mode "custom")

	// --- F4: Trailing Stop ---------------------------------------------
	// Setelah profit >= TrailingTriggerPct, SL dipindah ke entry (BEP), lalu
	// mengikuti harga: SL = harga ekstrem - TrailingDistPct. SL hanya bergerak
	// menguntungkan (naik untuk LONG, turun untuk SHORT), tidak pernah mundur.
	TrailingEnabled    bool    `json:"trailingEnabled"`
	TrailingTriggerPct float64 `json:"trailingTriggerPct"` // % profit untuk aktivasi (BEP)
	TrailingDistPct    float64 `json:"trailingDistPct"`    // % jarak trailing dari ekstrem
}

// Default TP/SL: rasio risiko:hadiah 1:2 yang BENAR.
// Stop loss 0.4% (risiko), take profit 0.8% (hadiah 2x risiko).
// Break-even win rate = 0.4 / (0.4 + 0.8) = 33.3%
const (
	defaultTakeProfitPct = 0.8 // 0.8% take profit  (hadiah)
	defaultStopLossPct   = 0.4 // 0.4% stop loss    (risiko)
)

// --- Metode ATR (Average True Range) untuk penempatan SL/TP ---
const (
	tpSlModePercent = "percent"
	tpSlModeATR     = "atr"

	defaultAtrPeriod = 14  // periode ATR standar
	defaultAtrSlMult = 1.0 // SL = 1 x ATR

	// Batas jarak SL berbasis ATR (dalam % harga) supaya level tidak ekstrem:
	// terlalu dekat (pasti kena noise/spread antar tick) atau terlalu jauh
	// (bisa melebihi margin sampai likuidasi). minAtrSlPct juga berlaku untuk
	// pengali ATR yang menghasilkan jarak sangat kecil di pasar yang sepi.
	minAtrSlPct = 0.25 // SL minimal 0.25% dari entry
	maxAtrSlPct = 5.0  // SL maksimal 5% dari entry (hard cap)
)

// --- F2: Daily loss limit → shutdown -------------------------------------
const (
	defaultLossLimitValue = 5.0 // default 5%

	// lossLimitCooldown menahan ulang tutup-posisi bila OKX menolak close
	// (mis. margin tidak cukup), supaya tidak membombardir API tiap 10 detik.
	lossLimitCooldown = 5 * time.Minute
)

// --- F3: Time filter — jendela sesi pasar dalam UTC -----------------------
// Waktu dibuat dalam UTC (acuan utama); konversi ke WIB (UTC+7) hanya
// untuk tampilan. Definisi sesi adalah perkiraan umum:
//
//	Asian 00:00-08:00, London 08:00-16:00, New York 13:00-21:00, overlap 13:00-16:00 UTC.
const (
	timeFilter247     = "24/7"
	timeFilterAsian   = "asian"
	timeFilterLondon  = "london"
	timeFilterNewYork = "newyork"
	timeFilterOverlap = "overlap"
	timeFilterCustom  = "custom"

	// Jam mulai/selesai sesi (UTC, jam 0-23).
	asianStart, asianEnd     = 0, 8
	londonStart, londonEnd   = 8, 16
	nyStart, nyEnd           = 13, 21
	overlapStart, overlapEnd = 13, 16
)

// --- F4: Trailing stop ----------------------------------------------------
const (
	// trailingMinDistPct: jarak trailing minimal > biaya round-trip OKX
	// (~0.16%), agar stop-loss tidak tersentuh hanya karena biaya transaksi.
	trailingMinDistPct = 0.17

	// trailingMaxDistPct: batas wajar atas jarak trailing.
	trailingMaxDistPct = 20.0
)

// sessionWindow mengembalikan jendela sesi (jam UTC, 0-23) untuk mode filter.
func sessionWindow(cfg Config) (start, end int) {
	switch cfg.TimeFilterMode {
	case timeFilterAsian:
		return asianStart, asianEnd
	case timeFilterLondon:
		return londonStart, londonEnd
	case timeFilterNewYork:
		return nyStart, nyEnd
	case timeFilterOverlap:
		return overlapStart, overlapEnd
	case timeFilterCustom:
		return cfg.CustomStartHour, cfg.CustomEndHour
	default: // "24/7" dan nilai tak dikenal -> selalu terbuka
		return 0, 24
	}
}

// timeFilterActive mengembalikan true bila saat ini (UTC) berada di dalam
// jendela sesi yang dipilih. Hanya dipakai untuk posisi BARU; posisi terbuka
// tetap dikelola apa pun hasilnya. Mendukung jendela lintas tengah malam
// (mis. custom 22:00 - 02:00 → start > end).
func timeFilterActive(cfg Config) bool {
	start, end := sessionWindow(cfg)
	if start == 0 && end == 24 {
		return true // "24/7"
	}
	now := time.Now().UTC()
	h := float64(now.Hour()) + float64(now.Minute())/60.0
	if start < end {
		// Jendela normal dalam satu hari.
		return h >= float64(start) && h < float64(end)
	}
	// Jendela lintas tengah malam (start > end).
	return h >= float64(start) || h < float64(end)
}

// pctToFrac mengubah persen menjadi fraksi untuk perkalian harga.
// 0.8 (persen) -> 0.008 (fraksi). Satu-satunya tempat konversi ini dilakukan.
func pctToFrac(pct float64) float64 { return pct / 100.0 }

// RiskReward mengembalikan rasio risiko:hadiah dalam bentuk "1:2.00".
// Nilai > 1 berarti configuration salah (risiko lebih besar dari hadiah).
func (c Config) RiskReward() float64 {
	if c.StopLossPct <= 0 {
		return 0
	}
	return c.TakeProfitPct / c.StopLossPct
}

// BreakEvenWinRate mengembalikan win rate minimum agar expectancy tidak negatif.
func (c Config) BreakEvenWinRate() float64 {
	tp, sl := c.TakeProfitPct, c.StopLossPct
	if tp+sl <= 0 {
		return 0
	}
	return sl / (tp + sl) * 100
}

type CoinState struct {
	Symbol         string    `json:"symbol"`
	Price          float64   `json:"price"`
	Position       string    `json:"position"`
	EntryPrice     float64   `json:"entryPrice"`
	PnL            float64   `json:"pnl"`
	Contracts      float64   `json:"contracts"`    // ukuran posisi AKTUAL di OKX (bukan ukuran order)
	MinMarginReq   float64   `json:"minMarginReq"` // margin minimum untuk 1 lot order
	IsValidSize    bool      `json:"isValidSize"`
	Signal         string    `json:"signal"`
	LastSignalTime time.Time `json:"-"`

	// Preview konfigurasi order (paling penting: inilah yang akan dikirim ke OKX)
	OrderSize    float64 `json:"orderSize"`    // jumlah kontrak yang akan dikirim
	ActualMargin float64 `json:"actualMargin"` // margin USDT yang benar-benar terpakai
	Notional     float64 `json:"notional"`     // nilai posisi = orderSize * price * ctVal
	Leverage     int     `json:"leverage"`
	TpPrice      float64 `json:"tpPrice"`
	SlPrice      float64 `json:"slPrice"`
	HasTPSL      bool    `json:"hasTpSl"` // apakah TP/SL sudah terpasang di OKX
	WsError      string  `json:"wsError"` // contoh: instrumen tidak tersedia di demo

	// Jarak TP/SL dari harga entry dalam persen (dipakai UI per-koin).
	// Mode persen: selalu sama dengan konfigurasi. Mode ATR: dinamis per posisi.
	TpDistPct float64 `json:"tpDistPct"`
	SlDistPct float64 `json:"slDistPct"`
	TpSlMode  string  `json:"tpSlMode"` // mode yang terpakai saat TP/SL dipasang
	AtrUsed   float64 `json:"atrUsed"`  // nilai ATR yang dipakai (0 = mode persen)

	// State trailing stop (F4). TrailActive=true artinya trigger profit sudah
	// tercapai dan SL dikelola dinamis. TrailExtreme = harga ekstrem sejak
	// entry (tertinggi utk LONG, terendah utk SHORT).
	TrailActive  bool    `json:"trailActive"`
	TrailExtreme float64 `json:"trailExtreme"`
}

type AppState struct {
	Config    Config
	Coins     map[string]*CoinState
	Logs      []string
	TotalPnL  float64
	StartTime time.Time
	mu        sync.RWMutex

	// Akun & proteksi harian (F1/F2). Diperbarui tiap sinkronisasi dari OKX.
	Balance  float64 `json:"balance"`  // total equity (USDT)
	DailyPnL float64 `json:"dailyPnL"` // realized + unrealized sejak 00:00 UTC
	// LossLimitHit mengunci engine setelah batas loss harian tercapai, sampai
	// reset 00:00 UTC berikutnya. LossDay = tanggal UTC (YYYY-MM-DD) dari
	// penghitungan DailyPnL saat ini.
	LossLimitHit bool      `json:"lossLimitHit"`
	LossDay      string    `json:"lossDay"`
	lastLossAct  time.Time // kapan terakhir aksi shutdown karena loss
}

func loadConfigFromEnv() Config {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("could not load .env: %v", err)
	}

	mode := strings.ToLower(strings.TrimSpace(os.Getenv("OKX_MODE")))
	if mode == "" {
		mode = "demo"
	}

	var apiKey, secretKey, passphrase string
	if mode == "live" {
		apiKey = os.Getenv("OKX_API_KEY")
		secretKey = os.Getenv("OKX_SECRET_KEY")
		passphrase = os.Getenv("OKX_PASSPHRASE")
	} else {
		apiKey = os.Getenv("OKX_DEMO_API_KEY")
		secretKey = os.Getenv("OKX_DEMO_SECRET_KEY")
		passphrase = os.Getenv("OKX_DEMO_PASSPHRASE")
	}

	return Config{
		ApiKey:             apiKey,
		SecretKey:          secretKey,
		Passphrase:         passphrase,
		Mode:               mode,
		MarginUSDT:         1.0,
		Leverage:           10,
		Coins:              [5]string{"BTC-USDT-SWAP", "ETH-USDT-SWAP", "SOL-USDT-SWAP", "DOGE-USDT-SWAP", "PEPE-USDT-SWAP"},
		IsRunning:          false,
		Timeframe:          "5m",
		TakeProfitPct:      defaultTakeProfitPct,
		StopLossPct:        defaultStopLossPct,
		TpSlMode:           tpSlModeATR,
		AtrPeriod:          defaultAtrPeriod,
		AtrSlMult:          defaultAtrSlMult,
		PosSizingMode:      "pct",
		LossLimitMode:      "pct",
		LossLimitValue:     defaultLossLimitValue,
		TimeFilterMode:     timeFilter247,
		TrailingTriggerPct: 0.5, // aktivasi trailing di profit >= 0.5%
		TrailingDistPct:    0.3, // jarak trailing 0.3% dari harga ekstrem
	}
}

var state = &AppState{
	Config: loadConfigFromEnv(),
	Coins:  make(map[string]*CoinState),
	Logs:   []string{"[SYSTEM] Bot initialized."},
}

// InstrumentSpec berisi spesifikasi kontrak resmi dari OKX.
// WAJIB diambil dari API (bukan di-hardcode) karena ctVal tiap koin berbeda
// dan salah ctVal = ukuran order & margin terpakai tidak sesuai dengan input user.
type InstrumentSpec struct {
	CtVal  float64 // jumlah base coin per 1 kontrak
	LotSz  float64 // kelipatan size order yang valid
	MinSz  float64 // ukuran order minimum
	TickSz float64 // kelipatan harga (tick size)
}

var (
	instrumentMu sync.RWMutex
	instruments  = make(map[string]InstrumentSpec)
)

// decimals untuk sebuah nilai tick (tickSz 0.1 -> 1, 0.0001 -> 4)
func decimalsFor(step float64) int {
	if step <= 0 {
		return 8
	}
	s := strconv.FormatFloat(step, 'f', -1, 64)
	if idx := strings.Index(s, "."); idx >= 0 {
		return len(s) - idx - 1
	}
	return 0
}

// loadInstrumentSpecs mengambil ctVal/lotSz/minSz/tickSz resmi dari OKX.
func loadInstrumentSpecs() {
	req, err := http.NewRequest("GET", "https://www.okx.com/api/v5/public/instruments?instType=SWAP", nil)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var parsed struct {
		Code string `json:"code"`
		Data []struct {
			InstId string `json:"instId"`
			CtVal  string `json:"ctVal"`
			LotSz  string `json:"lotSz"`
			MinSz  string `json:"minSz"`
			TickSz string `json:"tickSz"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || parsed.Code != "0" {
		return
	}

	specs := make(map[string]InstrumentSpec, len(parsed.Data))
	for _, it := range parsed.Data {
		ctVal, _ := strconv.ParseFloat(it.CtVal, 64)
		lotSz, _ := strconv.ParseFloat(it.LotSz, 64)
		minSz, _ := strconv.ParseFloat(it.MinSz, 64)
		tickSz, _ := strconv.ParseFloat(it.TickSz, 64)
		if ctVal <= 0 || lotSz <= 0 {
			continue
		}
		if minSz < lotSz {
			minSz = lotSz
		}
		specs[it.InstId] = InstrumentSpec{CtVal: ctVal, LotSz: lotSz, MinSz: minSz, TickSz: tickSz}
	}
	if len(specs) == 0 {
		return
	}

	instrumentMu.Lock()
	instruments = specs
	instrumentMu.Unlock()
	addLog(fmt.Sprintf("[SPEC] Loaded %d OKX instrument specs (ctVal/lotSz/tickSize)", len(specs)))
}

func getInstrumentSpec(symbol string) (InstrumentSpec, bool) {
	instrumentMu.RLock()
	spec, ok := instruments[symbol]
	instrumentMu.RUnlock()
	return spec, ok
}

// roundToPx membulatkan harga ke tick size OKX.
// dir = +1 round up (TP), -1 round down (SL)
func roundToPx(px, tick float64, dir int) float64 {
	if tick <= 0 {
		return px
	}
	var steps float64
	if dir < 0 {
		steps = math.Floor(px/tick + 1e-9)
	} else if dir > 0 {
		steps = math.Ceil(px/tick - 1e-9)
	} else {
		steps = math.Round(px / tick)
	}
	return steps * tick
}

// formatPx menghasilkan string harga dengan presisi tick size yang benar untuk payload OKX
func formatPx(px float64, spec InstrumentSpec) string {
	return strconv.FormatFloat(roundToPx(px, spec.TickSz, 0), 'f', decimalsFor(spec.TickSz), 64)
}

// ==========================================
// 2. OKX API & SIGNATURE ENGINE
// ==========================================

func generateSignature(timestamp, method, requestPath, body, secretKey string) string {
	message := timestamp + method + requestPath + body
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func okxRequest(method, requestPath, body string) (map[string]interface{}, error) {
	state.mu.RLock()
	cfg := state.Config
	state.mu.RUnlock()
	if cfg.ApiKey == "" || cfg.SecretKey == "" || cfg.Passphrase == "" {
		return nil, fmt.Errorf("OKX %s credentials are not configured", cfg.Mode)
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	sign := generateSignature(timestamp, method, requestPath, body, cfg.SecretKey)

	req, err := http.NewRequest(method, "https://www.okx.com"+requestPath, bytes.NewBufferString(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("OK-ACCESS-KEY", cfg.ApiKey)
	req.Header.Set("OK-ACCESS-SIGN", sign)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", cfg.Passphrase)
	req.Header.Set("Content-Type", "application/json")
	if cfg.Mode == "demo" {
		req.Header.Set("x-simulated-trading", "1")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if code, ok := result["code"].(string); ok && code != "0" {
		msg, _ := result["msg"].(string)
		if msg == "" {
			// OKX kadang mengirim code tanpa msg. Sertakan respons mentah agar
			// kesalahan tetap bisa didiagnosis dari log.
			if raw, mErr := json.Marshal(result); mErr == nil {
				return nil, fmt.Errorf("OKX API Error: %s - (pesan kosong, respons: %s)", code, raw)
			}
			return nil, fmt.Errorf("OKX API Error: %s - (pesan kosong)", code)
		}
		return nil, fmt.Errorf("OKX API Error: %s - %s", code, msg)
	}

	return result, nil
}

// ==========================================
// TP / SL
// ==========================================
//
// Nilai TP & SL TIDAK lagi konstanta global. Keduanya dibaca dari konfigurasi
// (Config.TakeProfitPct / Config.StopLossPct) sehingga bisa diubah dari UI dan
// selalu sama persis dengan yang dilihat pengguna.
//
// Arah harga juga handled di sini agar tidak ada tempat lain yang bisa salah:
//
//	LONG  → SL di bawah harga masuk, TP di atas
//	SHORT → SL di atas  harga masuk, TP di bawah
//
// Pembulatan memakai tick size OKX dengan arah yang menjamin rasio
// risiko:hadiah di OKX tidak pernah lebih buruk dari konfigurasi. Lihat
// komentar panjang pada computeTpSl.

// computeTpSl mengembalikan harga TP dan SL untuk sebuah posisi.
//
// SATUAN: tpPct dan slPct adalah PERSEN, bukan fraksi (0.8 berarti 0.8%).
// Konversi ke fraksi dilakukan DI DALAM fungsi ini agar tidak ada pemanggil
// yang bisa salah satuan. Contoh: entry 100, TP 0.8% -> 100.8 (bukan 180).
//
// ARAH PEMBULATAN (penting, jangan diubah sembarangan):
// Karena harga harus merupakan kelipatan tick size OKX, hasilnya tidak bisa
// persis sama dengan persentase konfigurasi. Arah pembulatan dipilih supaya
// rasio risiko:hadiah yang benar-benar terjadi di OKX TIDAK PERNAH lebih buruk
// dari yang dikonfigurasi:
//
//	TP dibulatkan ke arah yang MENJAUH harga masuk  -> hadiah >= tpPct
//	SL dibulatkan ke arah yang MENDEKATI harga masuk -> risiko  <= slPct
//
// Karena itu keduanya memakai arah yang sama: ke atas untuk LONG, ke bawah
// untuk SHORT. Contoh (ADA, entry 0.244786, tick 0.0001):
//
//	TP 0.2468 -> +0.823% (>= 0.8%, hadiah tidak kurang)
//	SL 0.2439 -> -0.362% (<= 0.4%, risiko tidak lebih)
//
// side: "buy" (long) atau "sell" (short)
func computeTpSl(side string, entryPrice float64, spec InstrumentSpec, tpPct, slPct float64) (tp, sl float64) {
	// Delegasi ke computeTpSlByMode (mode persen) supaya pembulatan tick size
	// punya SATU implementasi — mode ATR memakai jalur yang sama.
	calc := computeTpSlByMode(TpSlConfig{Mode: tpSlModePercent, TpPct: tpPct, SlPct: slPct}, side, entryPrice, spec, nil)
	return calc.TpPx, calc.SlPx
}

// ==========================================
// METODE PENEMPATAN SL/TP: PERSENTASE vs ATR
// ==========================================

// atrFromCandles menghitung nilai ATR terakhir dari rentetan candle OHLC.
// Tidak mengunci state — pemanggil menyalin candle lebih dulu.
func atrFromCandles(candles []CandleOHLC, period int) (float64, bool) {
	if period < 2 {
		return 0, false
	}
	n := len(candles)
	if n < period+1 {
		return 0, false
	}
	high := make([]float64, n)
	low := make([]float64, n)
	closePx := make([]float64, n)
	for i, c := range candles {
		high[i] = c.High
		low[i] = c.Low
		closePx[i] = c.Close
	}
	_, atr := indicator.Atr(period, high, low, closePx)
	if len(atr) == 0 {
		return 0, false
	}
	last := atr[len(atr)-1]
	if last <= 0 || math.IsNaN(last) || math.IsInf(last, 0) {
		return 0, false
	}
	return last, true
}

// atrValue membaca candle OHLC simbol (mengunci state.mu sendiri) lalu
// menghitung ATR. Aman dipanggil dari goroutine mana pun.
func atrValue(symbol string, period int) (float64, bool) {
	state.mu.RLock()
	candles := append([]CandleOHLC(nil), candleOHLC[symbol]...)
	state.mu.RUnlock()
	return atrFromCandles(candles, period)
}

// seedCandleHistory mengisi buffer candle dari REST OKX (up to 100 candle 5m).
// WS hanya mengirim candle yang baru terbentuk, jadi tanpa seeding ini ATR
// butuh ~70 menit untuk punya 14 candle. Dipanggil sekali per simbol saat
// startup / koin baru ditambahkan, dan menyesuaikan diri dengan data WS
// (tidak menimpa bila buffer sudah lebih lengkap dari yang bisa didapat).
func seedCandleHistory(symbol string) {
	url := fmt.Sprintf("https://www.okx.com/api/v5/market/candles?instId=%s&bar=5m&limit=100", symbol)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var parsed struct {
		Code string        `json:"code"`
		Data []interface{} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return
	}

	// Data datang dari candle TERBARU ke terlama — balikkan agar kronologis.
	closes := make([]float64, 0, 100)
	ohlcs := make([]CandleOHLC, 0, 100)
	for i := len(parsed.Data) - 1; i >= 0; i-- {
		arr, ok := parsed.Data[i].([]interface{})
		if !ok || len(arr) < 5 {
			continue
		}
		tsStr, _ := arr[0].(string)
		hStr, _ := arr[2].(string)
		lStr, _ := arr[3].(string)
		cStr, _ := arr[4].(string)
		ts, terr := strconv.ParseInt(tsStr, 10, 64)
		high, herr := strconv.ParseFloat(hStr, 64)
		low, lerr := strconv.ParseFloat(lStr, 64)
		closePx, cerr := strconv.ParseFloat(cStr, 64)
		if terr != nil || herr != nil || lerr != nil || cerr != nil || closePx <= 0 || high < low || low <= 0 {
			continue
		}
		closes = append(closes, closePx)
		ohlcs = append(ohlcs, CandleOHLC{Ts: ts, High: high, Low: low, Close: closePx})
	}
	if len(closes) == 0 {
		return
	}

	state.mu.Lock()
	if len(candleData[symbol]) < len(closes) {
		candleData[symbol] = closes
		candleOHLC[symbol] = ohlcs
	}
	state.mu.Unlock()
	addLog(fmt.Sprintf("[CANDLE HISTORY] %s: %d candle 5m dimuat (ATR siap)", symbol, len(closes)))
}

// TpSlConfig adalah snapshot konfigurasi TP/SL yang dipakai dalam SATU
// perhitungan, sehingga harga di UI, di order, dan di watchdog selalu identik.
type TpSlConfig struct {
	Mode      string  // "percent" | "atr"
	TpPct     float64 // persen (mode percent / fallback bila ATR belum ada)
	SlPct     float64
	AtrPeriod int
	AtrSlMult float64
	Leverage  int
}

func snapshotTpSlConfig() TpSlConfig {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return TpSlConfig{
		Mode:      state.Config.TpSlMode,
		TpPct:     state.Config.TakeProfitPct,
		SlPct:     state.Config.StopLossPct,
		AtrPeriod: state.Config.AtrPeriod,
		AtrSlMult: state.Config.AtrSlMult,
		Leverage:  state.Config.Leverage,
	}
}

// TpSlCalc adalah hasil perhitungan harga TP/SL untuk satu posisi.
type TpSlCalc struct {
	TpPx, SlPx  float64
	TpDistPct   float64 // jarak TP dari entry dalam % (hadiah)
	SlDistPct   float64 // jarak SL dari entry dalam % (risiko)
	Mode        string  // mode yang benar-benar terpakai
	AtrVal      float64 // ATR yang dipakai (0 bila bukan mode ATR)
	UsedPercent bool    // fallback ke persen karena ATR belum tersedia
	SlWasCapped bool    // jarak SL dibatasi (cegah SL melewati likuidasi)
}

// atrSlCapPct membatasi jarak SL berbasis ATR agar tidak sampai melewati titik
// likuidasi. Perkiraan jarak likuidasi isolated adalah ~100%/leverage; SL
// dibatasi 60% dari jarak itu supaya SL SELALU tereksekusi sebelum likuidasi.
func atrSlCapPct(leverage int) float64 {
	capPct := maxAtrSlPct
	if leverage > 1 {
		if liqCap := 100.0 / float64(leverage) * 0.6; liqCap < capPct {
			capPct = liqCap
		}
	}
	if capPct < minAtrSlPct {
		capPct = minAtrSlPct
	}
	return capPct
}

// clampAtrSlPct menjepit jarak SL (dalam %) ke rentang [minAtrSlPct, cap].
// Mengembalikan true bila nilai dijepit (berarti batas aktif).
func clampAtrSlPct(pct float64, leverage int) (float64, bool) {
	lo := minAtrSlPct
	hi := atrSlCapPct(leverage)
	if pct < lo {
		return lo, true
	}
	if pct > hi {
		return hi, true
	}
	return pct, false
}

// computeTpSlByMode adalah SATU-SATUNYA fungsi penempatan harga TP/SL untuk
// semua jalur (order baru, watchdog, reprice konfigurasi).
//
// mode "percent": jarak = persen konfigurasi (R:R sesuai input user).
// mode "atr":     SL = clamp(ATR x AtrSlMult) % di luar entry — level mengikuti
//
//	volatilitas pasar terkini; TP = 2 x jarak SL sehingga rasio
//	risiko:hadiah SELALU 1:2, dinamis di pasar sepi maupun ramai.
//	Bila data ATR belum cukup, fallback ke persen (UsedPercent).
//
// Pembulatan memakai invariant yang sama dengan computeTpSl: TP membulat ke
// arah MENJAUH entry (hadiah tidak pernah kurang), SL membulat ke arah
// MENDEKATI entry (risiko tidak pernah lebih).
func computeTpSlByMode(cfg TpSlConfig, side string, entryPrice float64, spec InstrumentSpec, candles []CandleOHLC) TpSlCalc {
	mode := cfg.Mode
	if mode != tpSlModeATR {
		mode = tpSlModePercent
	}

	slDistPct := cfg.SlPct
	tpDistPct := cfg.TpPct
	atrVal := 0.0
	usedPercent := false
	capped := false

	if mode == tpSlModeATR {
		var ok bool
		atrVal, ok = atrFromCandles(candles, cfg.AtrPeriod)
		if ok && atrVal > 0 && entryPrice > 0 {
			rawPct := atrVal / entryPrice * 100 * cfg.AtrSlMult
			slDistPct, capped = clampAtrSlPct(rawPct, cfg.Leverage)
			tpDistPct = 2 * slDistPct
		} else {
			// ATR belum tersedia (candle baru terkumpul) -> pakai persen.
			mode = tpSlModePercent
			usedPercent = true
		}
	}

	dir := 1.0 // long
	if side != "buy" {
		dir = -1.0 // short
	}
	step := int(dir) // TP dan SL sama-sama ke atas (long) / ke bawah (short)
	tpFrac := pctToFrac(tpDistPct)
	slFrac := pctToFrac(slDistPct)
	tpPx := roundToPx(entryPrice*(1+dir*tpFrac), spec.TickSz, step)
	slPx := roundToPx(entryPrice*(1-dir*slFrac), spec.TickSz, step)

	return TpSlCalc{
		TpPx: tpPx, SlPx: slPx,
		TpDistPct: tpDistPct, SlDistPct: slDistPct,
		Mode: mode, AtrVal: atrVal,
		UsedPercent: usedPercent, SlWasCapped: capped,
	}
}

func setLeverage(symbol string, leverage int) error {
	payload := map[string]interface{}{
		"instId":  symbol,
		"lever":   fmt.Sprintf("%d", leverage),
		"mgnMode": "isolated",
	}
	bodyBytes, _ := json.Marshal(payload)
	_, err := okxRequest("POST", "/api/v5/account/set-leverage", string(bodyBytes))
	return err
}

// OrderResult berisi informasi order yang berhasil dieksekusi OKX.
type OrderResult struct {
	OrderID string `json:"ordId"`
	SZ      string `json:"sz"`
}

// placeOrder mengirim order market + TP & SL untuk 100% posisi.
//
// PENTING: OKX MEMBAGI ukuran posisi jika TP dan SL diletakkan pada SATU elemen
// attachAlgoOrds (setengahnya untuk TP, setengahnya untuk SL). Karena itu TP dan SL
// dikirim sebagai DUA elemen TERPISAH, masing-masing memakai ukuran order penuh (100%).
//
// tpOrdPx/slOrdPx = "-1" berarti order TP/SL dieksekusi di harga market saat trigger
// tersentuh, sehingga tidak terkena error presisi tick size dari OKX.
func placeOrder(symbol, side string, size float64, entryPrice, tpPrice, slPrice float64) (OrderResult, error) {
	var res OrderResult

	spec, ok := getInstrumentSpec(symbol)
	if !ok {
		return res, fmt.Errorf("spesifikasi instrumen %s belum dimuat dari OKX", symbol)
	}
	if size < spec.MinSz {
		return res, fmt.Errorf("size %.4f kontrak di bawah minSz %.4f", size, spec.MinSz)
	}
	// Jaga agar nilai tidak masuk akal (mis. salah satuan) tidak pernah sampai
	// ke OKX. Harga TP/SL dihitung oleh computeTpSlByMode, di sini hanya
	// memastikan arah dan kelayakannya.
	if tpPrice <= 0 || slPrice <= 0 || math.IsNaN(tpPrice) || math.IsNaN(slPrice) {
		return res, fmt.Errorf("harga TP/SL tidak valid (tp=%v sl=%v)", tpPrice, slPrice)
	}
	if entryPrice <= 0 {
		return res, fmt.Errorf("entry price tidak valid: %v", entryPrice)
	}
	if side == "buy" && (tpPrice <= entryPrice || slPrice >= entryPrice) {
		return res, fmt.Errorf("arah TP/SL tidak cocok untuk LONG (entry=%v tp=%v sl=%v)", entryPrice, tpPrice, slPrice)
	}
	if side != "buy" && (tpPrice >= entryPrice || slPrice <= entryPrice) {
		return res, fmt.Errorf("arah TP/SL tidak cocok untuk SHORT (entry=%v tp=%v sl=%v)", entryPrice, tpPrice, slPrice)
	}

	szStr := strconv.FormatFloat(size, 'f', decimalsFor(spec.LotSz), 64)

	payload := map[string]interface{}{
		"instId":  symbol,
		"tdMode":  "isolated",
		"side":    side,
		"ordType": "market",
		"sz":      szStr,
		"attachAlgoOrds": []map[string]interface{}{
			{ // TP untuk 100% posisi
				"tpTriggerPx":     formatPx(tpPrice, spec),
				"tpOrdPx":         "-1",
				"tpTriggerPxType": "last",
			},
			{ // SL untuk 100% posisi
				"slTriggerPx":     formatPx(slPrice, spec),
				"slOrdPx":         "-1",
				"slTriggerPxType": "last",
			},
		},
	}
	bodyBytes, _ := json.Marshal(payload)

	result, err := okxRequest("POST", "/api/v5/trade/order", string(bodyBytes))
	if err != nil {
		return res, err
	}
	if data, ok := result["data"].([]interface{}); ok && len(data) > 0 {
		if first, ok := data[0].(map[string]interface{}); ok {
			res.OrderID, _ = first["ordId"].(string)
			res.SZ, _ = first["sz"].(string)
		}
	}
	return res, nil
}

// closePosition menutup 100% posisi memakai /api/v5/trade/close-position.
// Endpoint ini menutup SELURUH posisi di instrumen tersebut tanpa perlu menghitung
// ukuran kontrak, jadi mustahil terjadi partial close karena salah size.
func closePosition(symbol, position string) error {
	payload := map[string]interface{}{
		"instId":  symbol,
		"mgnMode": "isolated",
	}
	if position != "" {
		payload["posSide"] = position // long / short
	}
	bodyBytes, _ := json.Marshal(payload)
	_, err := okxRequest("POST", "/api/v5/trade/close-position", string(bodyBytes))
	return err
}

// pendingTpSlInfo merangkum order TP/SL conditional yang live di OKX.
type pendingTpSlInfo struct {
	HaveTp bool
	HaveSl bool
	TpPx   float64
	SlPx   float64
	TpSize float64
	SlSize float64
}

// fetchPendingTpSl membaca order TP/SL conditional yang live di OKX untuk satu instrumen.
func fetchPendingTpSl(symbol string) (pendingTpSlInfo, error) {
	var info pendingTpSlInfo
	// WAJIB: OKX mensyaratkan parameter ordType untuk endpoint ini (51000 tanpa itu)
	path := fmt.Sprintf("/api/v5/trade/orders-algo-pending?ordType=conditional&instType=SWAP&instId=%s", symbol)
	result, err := okxRequest("GET", path, "")
	if err != nil {
		return info, err
	}
	data, _ := result["data"].([]interface{})
	for _, item := range data {
		algo, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		// PENTING: field dari orders-algo-pending adalah "ordType" (bukan "algoType")
		ordType, _ := algo["ordType"].(string)
		instId, _ := algo["instId"].(string)
		stateStr, _ := algo["state"].(string)
		if ordType != "conditional" || instId != symbol || stateStr != "live" {
			continue
		}
		sz, _ := strconv.ParseFloat(toString(algo["sz"]), 64)
		if p, err := strconv.ParseFloat(toString(algo["tpTriggerPx"]), 64); err == nil {
			info.HaveTp, info.TpPx, info.TpSize = true, p, sz
		}
		if p, err := strconv.ParseFloat(toString(algo["slTriggerPx"]), 64); err == nil {
			info.HaveSl, info.SlPx, info.SlSize = true, p, sz
		}
	}
	return info, nil
}

// toString mengubah nilai JSON (string atau angka) menjadi string.
func toString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case nil:
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// tpSlSizeOK memeriksa apakah TP dan SL yang live masing-masing menutup
// SELURUH posisi (bukan sebagian).
const tpSlSizeEps = 1e-9

func tpSlSizeOK(info pendingTpSlInfo, positionSize float64) bool {
	return math.Abs(info.TpSize-positionSize) <= tpSlSizeEps && math.Abs(info.SlSize-positionSize) <= tpSlSizeEps
}

// tpSlMatches memeriksa apakah TP/SL yang live di OKX sudah benar-benar sama
// dengan yang diminta konfigurasi: harga trigger dan ukuran 100% posisi.
//
// Toleransi harga sengaja dibuat SANGAT KETAT (1/1000 tick size), bukan satu
// tick penuh. Satu tick penuh untuk ADA berarti 0.041% harga, yang cukup untuk
// membuat risiko sebenarnya 0.403% padahal konfigurasi 0.400%. Dengan
// toleransi ketat, selisih sekecil apa pun yang melanggar invarian
// (hadiah >= tpPct dan risiko <= slPct) akan diperbaiki.
func tpSlMatches(info pendingTpSlInfo, spec InstrumentSpec, positionSize, tpPrice, slPrice float64) bool {
	if !info.HaveTp || !info.HaveSl {
		return false
	}
	tol := spec.TickSz / 1000
	if tol < 1e-12 {
		tol = 1e-12
	}
	if math.Abs(info.TpPx-tpPrice) > tol {
		return false
	}
	if math.Abs(info.SlPx-slPrice) > tol {
		return false
	}
	// TP dan SL masing-masing harus menutup SELURUH posisi, bukan sebagian.
	return tpSlSizeOK(info, positionSize)
}

// tpSlFixCooldown mencegah bot memasang ulang TP/SL berulang kali bila OKX
// menolak order replacement. Mencegah churn API dan order bolak-balik.
const tpSlFixCooldown = 90 * time.Second

var (
	tpSlLockMu sync.Mutex
	tpSlLocks  = map[string]*sync.Mutex{}
	tpSlFixed  = map[string]time.Time{}
)

// acquireTpSlLock memastikan hanya satu proses yang memperbaiki TP/SL sebuah
// instrumen pada satu waktu (dipanggil setiap 10 detik dari sinkronisasi).
func acquireTpSlLock(symbol string) bool {
	tpSlLockMu.Lock()
	defer tpSlLockMu.Unlock()
	mu, ok := tpSlLocks[symbol]
	if !ok {
		mu = &sync.Mutex{}
		tpSlLocks[symbol] = mu
	}
	return mu.TryLock()
}

func releaseTpSlLock(symbol string) {
	tpSlLockMu.Lock()
	mu := tpSlLocks[symbol]
	tpSlLockMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// tpslRecentlyFixed melapor apakah TP/SL instrumen baru saja diperbaiki,
// sehingga percobaan perbaikan berulang dapat dilewati.
func tpslRecentlyFixed(symbol string) bool {
	tpSlLockMu.Lock()
	defer tpSlLockMu.Unlock()
	last, ok := tpSlFixed[symbol]
	return ok && time.Since(last) < tpSlFixCooldown
}

func markTpSlFixed(symbol string) {
	tpSlLockMu.Lock()
	tpSlFixed[symbol] = time.Now()
	tpSlLockMu.Unlock()
}

// ensureTPSL adalah jaring pengaman TP/SL yang berjalan setiap sinkronisasi posisi.
//
// Bedanya dengan sekadar "apakah TP/SL ada": fungsi ini membandingkan TP/SL yang
// live di OKX dengan harga yang seharusnya, lalu memperbaiki bila berbeda.
//
// Perilaku per mode:
//   - percent: harga yang diharapkan dihitung dari persen konfigurasi setiap
//     sinkronisasi, sehingga drift harga trigger langsung diperbaiki.
//   - atr: harga yang diharapkan adalah harga yang TERCATAT saat terakhir
//     dipasang (CoinState.TpPrice/SlPrice). Nilai ini stabil — TIDAK dihitung
//     ulang dari ATR tiap candle, karena ATR berubah terus dan itu akan
//     menyebabkan churn pemasangan ulang.
//
// Tanpa catatan (posisi dari sebelum restart / dibuka manual): bila TP/SL sudah
// terpasang 100% di OKX, level OKX DITERIMA apa adanya dan dicatat — tanpa
// perbaikan harga. Ini mencegah bot "menebak" level ATR yang berbeda dari yang
// pernah dipasang, yang hanya akan memicu pemasangan ulang tak perlu. Perbaikan
// hanya dilakukan bila TP/SL hilang atau ukurannya bukan 100% posisi.
func ensureTPSL(symbol, position string, positionSize, entryPrice float64, cfg TpSlConfig) {
	spec, ok := getInstrumentSpec(symbol)
	if !ok {
		return
	}
	if cfg.TpPct <= 0 || cfg.SlPct <= 0 || cfg.TpPct > 50 || cfg.SlPct > 50 {
		addLog(fmt.Sprintf("[WARN] TP/SL nonvalid (tp=%.3f%% sl=%.3f%%) untuk %s, watchdog dilewati", cfg.TpPct, cfg.SlPct, symbol))
		return
	}
	if positionSize < spec.MinSz {
		addLog(fmt.Sprintf("[WARN] Posisi %s %.4f kontrak terlalu kecil untuk TP/SL manual (min %.4f)", symbol, positionSize, spec.MinSz))
		return
	}

	// Arah side untuk order-algo: long -> sell, short -> buy
	computeSide := "buy"
	if position == "short" {
		computeSide = "sell"
	}

	// Baca state: candle OHLC (untuk ATR) + harga TP/SL yang TERCATAT.
	state.mu.RLock()
	candles := append([]CandleOHLC(nil), candleOHLC[symbol]...)
	var recTp, recSl, recTpDist, recSlDist, recAtr float64
	recMode := ""
	if coin := state.Coins[symbol]; coin != nil {
		recTp, recSl = coin.TpPrice, coin.SlPrice
		recTpDist, recSlDist = coin.TpDistPct, coin.SlDistPct
		recAtr, recMode = coin.AtrUsed, coin.TpSlMode
	}
	state.mu.RUnlock()
	hadRecord := recTp > 0 && recSl > 0

	// Harga yang diharapkan. Mode ATR + ada catatan -> anchor ke catatan
	// (stabil, anti-churn). Jarak yang tercatat ikut dibawa agar display
	// R:R per-koin tidak hilang bila perbaikan terpaksa terjadi.
	calc := computeTpSlByMode(cfg, computeSide, entryPrice, spec, candles)
	if calc.Mode == tpSlModeATR && hadRecord {
		if recMode == "" {
			recMode = tpSlModeATR
		}
		calc = TpSlCalc{TpPx: recTp, SlPx: recSl, Mode: recMode, AtrVal: recAtr}
		if recTpDist > 0 && recSlDist > 0 {
			calc.TpDistPct, calc.SlDistPct = recTpDist, recSlDist
		}
	}

	// Cegah dua sinkronisasi bersamaan memperbaiki instrumen yang sama.
	if !acquireTpSlLock(symbol) {
		return
	}
	defer releaseTpSlLock(symbol)

	info, err := fetchPendingTpSl(symbol)
	if err != nil {
		addLog(fmt.Sprintf("[WARN] Gagal cek TP/SL %s: %v", symbol, err))
		return
	}

	// Tanpa catatan + TP/SL sudah live 100%: adopsi level OKX apa adanya.
	// Label mode mengikuti konfigurasi aktif (level yang diadopsi diperiksa
	// ulang ke mode ATR saat konfigurasi TP/SL berikutnya diubah dari UI).
	if !hadRecord && info.HaveTp && info.HaveSl && tpSlSizeOK(info, positionSize) {
		state.mu.Lock()
		if coin := state.Coins[symbol]; coin != nil {
			coin.HasTPSL = true
			coin.TpPrice = info.TpPx
			coin.SlPrice = info.SlPx
			coin.TpSlMode = cfg.Mode
			if entryPrice > 0 {
				coin.TpDistPct = math.Abs(info.TpPx-entryPrice) / entryPrice * 100
				coin.SlDistPct = math.Abs(info.SlPx-entryPrice) / entryPrice * 100
			}
		}
		state.mu.Unlock()
		addLog(fmt.Sprintf("[TP/SL] %s: adopsi level OKX yang sudah ada (TP %s / SL %s, 100%% posisi). Bila ingin level ATR baru, ubah konfigurasi TP/SL dari UI.",
			symbol, formatPx(info.TpPx, spec), formatPx(info.SlPx, spec)))
		return
	}

	// Sudah benar: TP dan SL live, harga sesuai, keduanya 100% posisi.
	if tpSlMatches(info, spec, positionSize, calc.TpPx, calc.SlPx) {
		state.mu.Lock()
		if coin := state.Coins[symbol]; coin != nil {
			coin.HasTPSL = true
			coin.TpPrice = info.TpPx
			coin.SlPrice = info.SlPx
		}
		state.mu.Unlock()
		return
	}

	// Ada yang tidak cocok. Kalau baru saja diperbaiki, tunggu cooldown supaya
	// tidak membombardir OKX bila penolakan order terus terjadi.
	if tpslRecentlyFixed(symbol) {
		return
	}

	reason := "tidak ada"
	switch {
	case !info.HaveTp && !info.HaveSl:
		reason = "tidak ada TP/SL sama sekali"
	case !info.HaveTp:
		reason = "TP hilang"
	case !info.HaveSl:
		reason = "SL hilang"
	default:
		reason = fmt.Sprintf("harga/ukuran tidak cocok (OKX TP=%s SL=%s, expected TP=%s SL=%s)",
			formatPx(info.TpPx, spec), formatPx(info.SlPx, spec), formatPx(calc.TpPx, spec), formatPx(calc.SlPx, spec))
	}
	addLog(fmt.Sprintf("[TP/SL] Menyesuaikan %s: %s", symbol, reason))

	// PENTING: perbaikan harus lewat replaceTpSl, bukan memasang order tambahan.
	// Bila order lama tidak dibatalkan, TP/SL lama dan baru akan menumpuk
	// berjejang (terbukti: satu posisi pernah punya 4 algo order), dan level
	// basi bisa menutup posisi lebih dulu dengan harga yang salah.
	if !replaceTpSl(symbol, position, positionSize, calc) {
		return
	}
	markTpSlFixed(symbol)
}

// placeTpSlAlgos memasang order TP dan SL sebagai DUA algo order terpisah,
// masing-masing berukuran 100% posisi, reduceOnly, dan dieksekusi di market.
//
// Dua elemen terpisah itu wajib: kalau TP dan SL diletakkan pada satu elemen
// dengan ukuran penuh, OKX akan MEMBAGI ukuran posisi (setengah ke TP, setengah
// ke SL), sehingga exit tidak pernah menutup 100% posisi.
//
// Mengembalikan true hanya bila kedua order berhasil dibuat.
func placeTpSlAlgos(symbol, position string, positionSize, tpPrice, slPrice float64, spec InstrumentSpec) bool {
	// Pada mode NET, order-algo untuk menutup posisi memakai side buy/sell
	// (buy menutup short, sell menutup long). posSide tetap "net".
	exitSide := "sell"
	if position == "short" {
		exitSide = "buy"
	}
	szStr := strconv.FormatFloat(positionSize, 'f', decimalsFor(spec.LotSz), 64)

	allOK := true
	place := func(kind, trigger string) {
		body := map[string]interface{}{
			"instId":     symbol,
			"tdMode":     "isolated",
			"side":       exitSide,
			"posSide":    "net",
			"instType":   "SWAP",
			"sz":         szStr,
			"ordType":    "conditional", // WAJIB "conditional", bukan "algoType"
			"reduceOnly": "true",        // wajib: tanpa ini TP/SL bisa membuka posisi terbalik
		}
		if kind == "tp" {
			body["tpTriggerPx"] = trigger
			body["tpOrdPx"] = "-1" // dieksekusi di harga market saat trigger tersentuh
		} else {
			body["slTriggerPx"] = trigger
			body["slOrdPx"] = "-1"
		}
		payload, _ := json.Marshal(body)
		if _, err := okxRequest("POST", "/api/v5/trade/order-algo", string(payload)); err != nil {
			allOK = false
			addLog(fmt.Sprintf("[WARN] Gagal pasang %s %s: %v", strings.ToUpper(kind), symbol, err))
		}
	}
	place("tp", formatPx(tpPrice, spec))
	place("sl", formatPx(slPrice, spec))
	return allOK
}

// replaceTpSl adalah SATU-SATUNYA jalur untuk memasang ulang TP/SL.
//
// Urutannya: batalkan semua algo order TP/SL lama untuk instrumen ini, tunggu
// OKX memproses, lalu pasang TP dan SL baru. Membatalkan lebih dulu adalah
// syarat agar tidak terjadi penumpukan order (sebelumnya bisa ada 4 algo order
// untuk satu posisi) dan agar level lama tidak menutup posisi dengan harga
// yang sudah tidak sesuai konfigurasi.
//
// Harga TP/SL sudah dihitung pemanggil (computeTpSlByMode) — fungsi ini hanya
// memasang dan mencatatnya, supaya semua jalur memakai perhitungan yang sama.
//
// Mengembalikan true hanya bila TP dan SL baru sama-sama berhasil dipasang.
func replaceTpSl(symbol, position string, positionSize float64, calc TpSlCalc) bool {
	spec, ok := getInstrumentSpec(symbol)
	if !ok {
		return false
	}
	if positionSize < spec.MinSz {
		return false
	}
	if calc.TpPx <= 0 || calc.SlPx <= 0 || math.IsNaN(calc.TpPx) || math.IsNaN(calc.SlPx) {
		return false
	}

	// Batalkan algo order lama agar tidak ada TP/SL ganda yang bertumpuk.
	if err := cancelAlgoOrdersFor(symbol); err != nil {
		addLog(fmt.Sprintf("[WARN] Gagal batalkan TP/SL lama %s: %v", symbol, err))
	}
	// Beri jeda supaya OKX memproses pembatalan sebelum order baru dibuat.
	time.Sleep(600 * time.Millisecond)

	// Helper yang sama dipakai ensureTPSL, jadi tidak ada duplikasi logika order.
	if !placeTpSlAlgos(symbol, position, positionSize, calc.TpPx, calc.SlPx, spec) {
		addLog(fmt.Sprintf("[TP/SL] Gagal memasang TP/SL baru %s, posisi sedang tanpa proteksi penuh", symbol))
		return false
	}

	state.mu.Lock()
	if coin := state.Coins[symbol]; coin != nil {
		coin.HasTPSL = true
		coin.TpPrice = calc.TpPx
		coin.SlPrice = calc.SlPx
		coin.TpDistPct = calc.TpDistPct
		coin.SlDistPct = calc.SlDistPct
		coin.TpSlMode = calc.Mode
		coin.AtrUsed = calc.AtrVal
	}
	state.mu.Unlock()

	modeTag := strings.ToUpper(calc.Mode)
	if calc.UsedPercent {
		modeTag = "PERSEN (fallback, ATR belum cukup)"
	}
	rr := 0.0
	if calc.SlDistPct > 0 {
		rr = calc.TpDistPct / calc.SlDistPct
	}
	addLog(fmt.Sprintf("[TP/SL UPDATED] %s %s [%s] | TP: %s (+%.2f%%) | SL: %s (-%.2f%%) | 100%% posisi (%.4f kontrak) | R:R 1:%.2f",
		symbol, strings.ToUpper(position), modeTag, formatPx(calc.TpPx, spec), calc.TpDistPct,
		formatPx(calc.SlPx, spec), calc.SlDistPct, positionSize, rr))
	return true
}

// repriceOpenPositionsTpSl memasang ulang TP/SL untuk semua posisi yang sedang
// terbuka memakai konfigurasi baru, sehingga konfigurasi UI langsung berlaku.
// Pada mode ATR, harga dihitung dari ATR pasar TERKINI (aplikasi ulang disengaja
// dilakukan satu kali, bukan terus-menerus dari watchdog).
func repriceOpenPositionsTpSl(cfg TpSlConfig) {
	result, err := okxRequest("GET", "/api/v5/account/positions", "")
	if err != nil {
		addLog(fmt.Sprintf("[WARN] Gagal ambil posisi untuk update TP/SL: %v", err))
		return
	}
	data, _ := result["data"].([]interface{})

	type target struct {
		symbol, position string
		size, entry      float64
	}
	targets := make([]target, 0)
	for _, item := range data {
		pos, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		instId, _ := pos["instId"].(string)
		posSide, _ := pos["posSide"].(string)
		avgPxStr, _ := pos["avgPx"].(string)
		posStr, _ := pos["pos"].(string)
		avgPx, _ := strconv.ParseFloat(avgPxStr, 64)
		posSize, _ := strconv.ParseFloat(posStr, 64)
		if avgPx <= 0 || math.Abs(posSize) < 1e-12 {
			continue
		}
		targets = append(targets, target{instId, posSide, math.Abs(posSize), avgPx})
	}

	if len(targets) == 0 {
		return
	}
	addLog(fmt.Sprintf("[TP/SL] Menerapkan konfigurasi %s ke %d posisi terbuka (fallback TP %.2f%% / SL %.2f%%)",
		strings.ToUpper(cfg.Mode), len(targets), cfg.TpPct, cfg.SlPct))

	for _, t := range targets {
		// Lock yang sama dipakai watchdog, jadi tidak ada dua proses yang
		// membatalkan/memasang order TP/SL untuk instrumen ini bersamaan.
		if !acquireTpSlLock(t.symbol) {
			continue
		}
		spec, ok := getInstrumentSpec(t.symbol)
		if !ok {
			releaseTpSlLock(t.symbol)
			continue
		}
		computeSide := "buy"
		if t.position == "short" {
			computeSide = "sell"
		}
		state.mu.RLock()
		candles := append([]CandleOHLC(nil), candleOHLC[t.symbol]...)
		state.mu.RUnlock()
		calc := computeTpSlByMode(cfg, computeSide, t.entry, spec, candles)
		replaceTpSl(t.symbol, t.position, t.size, calc)
		markTpSlFixed(t.symbol)
		releaseTpSlLock(t.symbol)
	}
}

// ==========================================
// 2.6 AKUN, RISIKO & FILTER (F1-F4)
// ==========================================

// effectiveMargin menghitung margin per trade yang dipakai (F1 atau default).
//
//	F1 OFF        -> MarginUSDT (perilaku lama)
//	F1 "pct"      -> Saldo * PosSizingValue / 100
//	F1 "fixed"    -> PosSizingValue USDT
//
// Bila mode "pct" tapi saldo belum diketahui (0), fallback ke MarginUSDT agar
// order tidak gagal sebelum balance pertama tiba dari OKX.
func effectiveMargin(cfg Config, balance float64) float64 {
	if !cfg.PosSizingEnabled {
		return cfg.MarginUSDT
	}
	if cfg.PosSizingMode == "fixed" {
		return cfg.PosSizingValue
	}
	if balance <= 0 {
		return cfg.MarginUSDT
	}
	return balance * cfg.PosSizingValue / 100
}

// f1modeLabel menghasilkan label ringkas mode Position Sizing untuk log/UI.
func f1modeLabel(cfg Config) string {
	if !cfg.PosSizingEnabled {
		return "default (margin tetap)"
	}
	if cfg.PosSizingMode == "fixed" {
		return fmt.Sprintf("nominal %.2f USDT", cfg.PosSizingValue)
	}
	return fmt.Sprintf("%.2f%% saldo", cfg.PosSizingValue)
}

// fetchAccountBalance mengambil total equity akun (USDT) dari OKX.
func fetchAccountBalance() (float64, error) {
	result, err := okxRequest("GET", "/api/v5/account/balance", "")
	if err != nil {
		return 0, err
	}
	data, _ := result["data"].([]interface{})
	if len(data) == 0 {
		return 0, fmt.Errorf("account/balance tidak mengembalikan data")
	}
	acc, ok := data[0].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("account/balance format tidak dikenal")
	}
	totalEq, _ := strconv.ParseFloat(toString(acc["totalEq"]), 64)
	return totalEq, nil
}

// utcDayStartMillis mengembalikan unix-millis awal hari UTC ini.
// Semua perhitungan waktu memakai UTC sebagai acuan utama.
func utcDayStartMillis() int64 {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return start.UnixMilli()
}

// fetchDailyPnL menghitung PnL harian (USDT) sejak 00:00 UTC:
//
//	realized   = jumlah pnl fill hari ini (entry fill pnl = 0, tidak dobel)
//	unrealized = jumlah upl semua posisi terbuka saat ini
func fetchDailyPnL() (float64, error) {
	dayStart := utcDayStartMillis()
	realized := 0.0

	result, err := okxRequest("GET", "/api/v5/trade/fills?instType=SWAP&limit=100", "")
	if err != nil {
		return 0, err
	}
	if data, ok := result["data"].([]interface{}); ok {
		for _, item := range data {
			fill, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			ts, _ := strconv.ParseInt(toString(fill["ts"]), 10, 64)
			if ts < dayStart {
				continue // fill hari kemarin -> dihitung ulang setelah reset
			}
			pnl, _ := strconv.ParseFloat(toString(fill["pnl"]), 64)
			if feeCcy, _ := fill["feeCcy"].(string); feeCcy == "USDT" {
				if fee, err := strconv.ParseFloat(toString(fill["fee"]), 64); err == nil {
					pnl -= fee
				}
			}
			realized += pnl
		}
	}

	unreal := 0.0
	posResult, err := okxRequest("GET", "/api/v5/account/positions", "")
	if err != nil {
		return 0, err
	}
	if data, ok := posResult["data"].([]interface{}); ok {
		for _, item := range data {
			pos, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			upl, _ := strconv.ParseFloat(toString(pos["upl"]), 64)
			unreal += upl
		}
	}
	return realized + unreal, nil
}

// dailyLossLimit menghitung batas loss harian sesuai F2 (dan F1 sebagai basis):
//
//	mode "fixed" -> LossLimitValue USDT
//	mode "pct"   -> basis * LossLimitValue / 100, di mana
//	                basis = nilai Position Sizing (F1) bila F1 aktif,
//	                selain itu = saldo akun saat ini.
//
// Dipanggil setiap evaluasi sehingga perubahan F1/F2 langsung mengubah batas
// secara dinamis (sesuai PRD).
func dailyLossLimit(cfg Config, balance float64) float64 {
	if !cfg.LossLimitEnabled {
		return 0
	}
	if cfg.LossLimitMode == "fixed" {
		return cfg.LossLimitValue
	}
	base := cfg.MarginUSDT
	if cfg.PosSizingEnabled {
		base = effectiveMargin(cfg, balance)
	} else if balance > 0 {
		base = balance
	}
	return base * cfg.LossLimitValue / 100
}

// ensureLossDayResetLocked mereset state loss harian bila hari UTC berganti
// (00:00 UTC). PEMANGGIL wajib memegang state.mu (write lock).
func ensureLossDayResetLocked() {
	today := time.Now().UTC().Format("2006-01-02")
	if state.LossDay != today {
		state.LossDay = today
		state.LossLimitHit = false
		state.DailyPnL = 0
		state.lastLossAct = time.Time{}
	}
}

// shutdownForLoss menutup SEMUA posisi, membatalkan order TP/SL pending,
// menghentikan engine, dan mengunci start sampai reset 00:00 UTC.
// Mengembalikan true bila semua posisi berhasil ditutup (LossLimitHit di-set).
func shutdownForLoss(cfg Config) bool {
	state.mu.Lock()
	positions := make([]string, 0)
	for symbol, coin := range state.Coins {
		if coin.Position != "NONE" {
			positions = append(positions, symbol)
		}
	}
	state.Config.IsRunning = false
	state.mu.Unlock()

	if len(positions) == 0 {
		state.mu.Lock()
		state.LossLimitHit = true
		state.lastLossAct = time.Now()
		state.mu.Unlock()
		addLog("[LOSS LIMIT] Batas loss harian tercapai. Engine dihentikan (tidak ada posisi terbuka). Engine terkunci sampai 00:00 UTC.")
		return true
	}

	allClosed := true
	for _, symbol := range positions {
		if err := closePosition(symbol, ""); err != nil {
			allClosed = false
			addLog(fmt.Sprintf("[LOSS LIMIT ERROR] Gagal tutup %s: %v", symbol, err))
			continue
		}
		openPosMu.Lock()
		delete(openPositions, symbol)
		openPosMu.Unlock()
		state.mu.Lock()
		if coin := state.Coins[symbol]; coin != nil {
			coin.Position = "NONE"
			coin.EntryPrice = 0
			coin.Contracts = 0
			coin.HasTPSL = false
		}
		state.mu.Unlock()
		addLog(fmt.Sprintf("[LOSS LIMIT] Posisi %s ditutup penuh", symbol))
	}
	cancelPendingAlgoOrders("LOSS LIMIT")

	if allClosed {
		state.mu.Lock()
		state.LossLimitHit = true
		state.lastLossAct = time.Now()
		state.mu.Unlock()
		addLog("[LOSS LIMIT] Semua posisi ditutup dan engine dihentikan. Engine terkunci sampai reset 00:00 UTC.")
	} else {
		// Tetap catat waktu percobaan agar percobaan ulang dibatasi cooldown
		// (tidak membombardir OKX tiap 10 detik saat close gagal).
		state.mu.Lock()
		state.lastLossAct = time.Now()
		state.mu.Unlock()
		addLog("[LOSS LIMIT] Sebagian posisi gagal ditutup; percobaan akan diulang pada sinkronisasi berikutnya.")
	}
	return allClosed
}

// refreshAccountData memperbarui saldo akun + PnL harian dari OKX lalu
// mengevaluasi batas loss harian (F2). Dipanggil tiap 10 detik.
// Aksi shutdown hanya dieksekusi bila batas tercapai; log peringatan dibatasi
// supaya tidak membanjiri logbox saat OKX bermasalah.
var lastAccountWarn time.Time

func refreshAccountData() {
	balance, err := fetchAccountBalance()
	if err == nil {
		state.mu.Lock()
		state.Balance = balance
		state.mu.Unlock()
	} else if time.Since(lastAccountWarn) > 5*time.Minute {
		lastAccountWarn = time.Now()
		addLog(fmt.Sprintf("[WARN] Gagal ambil saldo akun: %v", err))
	}

	daily, err := fetchDailyPnL()
	if err != nil {
		if time.Since(lastAccountWarn) > 5*time.Minute {
			lastAccountWarn = time.Now()
			addLog(fmt.Sprintf("[WARN] Gagal hitung PnL harian: %v", err))
		}
		return
	}

	state.mu.Lock()
	ensureLossDayResetLocked()
	state.DailyPnL = daily
	cfg := state.Config
	hit := state.LossLimitHit
	lastAct := state.lastLossAct
	state.mu.Unlock()

	if !cfg.LossLimitEnabled || hit || daily > -1e-12 {
		return
	}
	limit := dailyLossLimit(cfg, balance)
	if limit <= 0 || daily > -limit {
		return
	}
	// Breach dipastikan berulang (cooldown) kalau tutup posisi gagal.
	if time.Since(lastAct) < lossLimitCooldown {
		return
	}
	addLog(fmt.Sprintf("[LOSS LIMIT] PnL harian %.4f USDT mencapai batas -%.4f USDT → shutdown otomatis", daily, limit))
	shutdownForLoss(cfg)
}

// roundTickDown membulatkan harga ke bawah ke kelipatan tick (LONG SL).
func roundTickDown(px, tick float64) float64 {
	if tick <= 0 {
		return px
	}
	return math.Floor(px/tick+1e-9) * tick
}

// roundTickUp membulatkan harga ke atas ke kelipatan tick (SHORT SL).
func roundTickUp(px, tick float64) float64 {
	if tick <= 0 {
		return px
	}
	return math.Ceil(px/tick-1e-9) * tick
}

// manageTrailing mengelola trailing stop (F4) untuk satu posisi terbuka.
// Dipanggil dari sinkronisasi posisi (tiap 10 detik) memakai harga mark OKX.
//
// Aturan PRD:
//  1. profit >= TriggerProfit% -> SL dipindah ke entry (break-even point).
//  2. harga terus bergerak      -> SL = harga ekstrem ± TrailingDistance.
//  3. SL hanya boleh bergerak MENGUNTUNGKAN (naik utk LONG, turun utk SHORT),
//     tidak pernah mundur.
//  4. profit belum mencapai trigger -> SL tetap (fixed dari ATR/persen, tidak
//     diubah), hanya harga ekstrem yang dicatat.
//
// Pembaruan lewat replaceTpSl (batal + pasang ulang TP & SL) dengan TP lama
// dipertahankan, dan memakai cooldown yang sama dengan watchdog supaya bot
// tidak membombardir OKX ketika harga naik terus menerus.
func manageTrailing(symbol, side string, positionSize, entryPrice, markPx float64, cfg Config) {
	if !cfg.TrailingEnabled || positionSize <= 0 || entryPrice <= 0 || markPx <= 0 {
		return
	}
	spec, ok := getInstrumentSpec(symbol)
	if !ok {
		return
	}

	// Jangan bentrok dengan watchdog / reprice yang sedang memperbaiki simbol ini.
	if !acquireTpSlLock(symbol) {
		return
	}
	defer releaseTpSlLock(symbol)

	info, err := fetchPendingTpSl(symbol)
	if err != nil || !info.HaveSl {
		return // tanpa SL terpasang, trailing tidak beroperasi
	}
	currentSL := info.SlPx

	state.mu.RLock()
	coin := state.Coins[symbol]
	trailActive := false
	extreme := markPx
	keptTP := info.TpPx
	keptTpDist := 0.0
	keptMode := cfg.TpSlMode
	keptAtr := 0.0
	if coin != nil {
		trailActive = coin.TrailActive
		if coin.TrailExtreme > 0 {
			extreme = coin.TrailExtreme
		}
		if coin.TpPrice > 0 {
			keptTP = coin.TpPrice
		}
		keptTpDist = coin.TpDistPct
		if coin.TpSlMode != "" {
			keptMode = coin.TpSlMode
		}
		keptAtr = coin.AtrUsed
	}
	state.mu.RUnlock()

	// Profit % dari entry ke mark (positif = profit).
	profitPct := 0.0
	if side == "short" {
		profitPct = (entryPrice - markPx) / entryPrice * 100
	} else {
		profitPct = (markPx - entryPrice) / entryPrice * 100
	}

	// Harga ekstrem sejak entry: tertinggi utk LONG, terendah utk SHORT.
	newExtreme := extreme
	if side == "short" {
		if markPx < newExtreme {
			newExtreme = markPx
		}
	} else {
		if markPx > newExtreme {
			newExtreme = markPx
		}
	}

	if !trailActive && profitPct < cfg.TrailingTriggerPct {
		// Trigger belum tercapai: catat ekstrem, biarkan SL fixed.
		state.mu.Lock()
		if c := state.Coins[symbol]; c != nil {
			c.TrailExtreme = newExtreme
		}
		state.mu.Unlock()
		return
	}

	// Hitung SL target. Saat baru aktif, SL pindah ke entry (BEP).
	var wantSL float64
	if side == "short" {
		if !trailActive {
			wantSL = entryPrice
		} else {
			wantSL = newExtreme * (1 + pctToFrac(cfg.TrailingDistPct))
		}
	} else {
		if !trailActive {
			wantSL = entryPrice
		} else {
			wantSL = newExtreme * (1 - pctToFrac(cfg.TrailingDistPct))
		}
	}
	// Bulatkan ke tick MENJAUH dari harga saat ini (LONG ke bawah, SHORT ke
	// atas) supaya jarak aktual >= TrailingDistPct dan tidak kena noise.
	if side == "short" {
		wantSL = roundTickUp(wantSL, spec.TickSz)
	} else {
		wantSL = roundTickDown(wantSL, spec.TickSz)
	}

	// Aturan mutlak: SL hanya boleh bergerak MENGUNTUNGKAN.
	if side == "short" {
		if wantSL > currentSL {
			wantSL = currentSL
		}
	} else {
		if wantSL < currentSL {
			wantSL = currentSL
		}
	}

	// Tidak ada perubahan berarti -> cukup perbarui state trailing.
	tol := spec.TickSz / 2
	if tol < 1e-12 {
		tol = 1e-12
	}
	state.mu.Lock()
	if c := state.Coins[symbol]; c != nil {
		c.TrailActive = true
		c.TrailExtreme = newExtreme
	}
	state.mu.Unlock()
	if math.Abs(wantSL-currentSL) <= tol {
		return
	}

	// Cooldown yang sama dengan watchdog: hindari pasang ulang berlebihan.
	if tpslRecentlyFixed(symbol) {
		return
	}

	slDist := math.Abs(wantSL-entryPrice) / entryPrice * 100
	calc := TpSlCalc{
		TpPx:        keptTP,
		SlPx:        wantSL,
		Mode:        keptMode,
		AtrVal:      keptAtr,
		TpDistPct:   keptTpDist,
		SlDistPct:   slDist,
		UsedPercent: keptMode != tpSlModeATR,
	}
	if !replaceTpSl(symbol, side, positionSize, calc) {
		return
	}
	markTpSlFixed(symbol)
	addLog(fmt.Sprintf("[TRAILING] %s %s: SL naik ke %s (ekstrem %s, jarak %.2f%%) — trailing aktif",
		symbol, strings.ToUpper(side), formatPx(wantSL, spec), formatPx(newExtreme, spec), cfg.TrailingDistPct))
}

// ==========================================
// 2.5 POSITION SYNC & MANAGEMENT
// ==========================================

// openPositions adalah peta internal untuk mendeteksi kapan bot menutup posisi,
// agar PnL yang ditampilkan dashboard sama dengan PnL riil OKX.
var openPositions = make(map[string]PositionInfo)
var openPosMu sync.Mutex

type PositionInfo struct {
	Side       string
	EntryPrice float64
	Size       float64
	Margin     float64
}

func syncPositionsWithOKX() {
	result, err := okxRequest("GET", "/api/v5/account/positions", "")
	if err != nil {
		addLog(fmt.Sprintf("[SYNC ERROR] %v", err))
		return
	}

	data, ok := result["data"].([]interface{})
	if !ok {
		return
	}

	// Kumpulkan posisi aktif di OKX
	live := make(map[string]PositionInfo)
	for _, item := range data {
		pos, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		instId, _ := pos["instId"].(string)
		posSide, _ := pos["posSide"].(string)
		avgPxStr, _ := pos["avgPx"].(string)
		posStr, _ := pos["pos"].(string)
		uplRatioStr, _ := pos["uplRatio"].(string)
		markPxStr, _ := pos["markPx"].(string)
		imrStr, _ := pos["imr"].(string)

		avgPrice, _ := strconv.ParseFloat(avgPxStr, 64)
		posSize, _ := strconv.ParseFloat(posStr, 64)
		uplRatio, _ := strconv.ParseFloat(uplRatioStr, 64)
		markPx, _ := strconv.ParseFloat(markPxStr, 64)
		imr, _ := strconv.ParseFloat(imrStr, 64)

		if avgPrice <= 0 || math.Abs(posSize) < 1e-12 {
			continue // posisi flat
		}

		side := "LONG"
		if posSide == "short" {
			side = "SHORT"
		}
		info := PositionInfo{Side: side, EntryPrice: avgPrice, Size: math.Abs(posSize), Margin: imr}
		live[instId] = info

		state.mu.Lock()
		coin := state.Coins[instId]
		isOrphan := coin == nil
		if isOrphan {
			// Ada posisi di OKX untuk koin yang tidak sedang dipantau (mis. leftovers).
			// Tetap tampilkan di dashboard agar user tahu ada posisi terbuka.
			coin = &CoinState{Symbol: instId, Position: "NONE"}
			state.Coins[instId] = coin
		}
		coin.Position = side
		coin.EntryPrice = avgPrice
		// Ukuran posisi AKTUAL dari OKX (bukan pratinjau order)
		coin.Contracts = info.Size
		coin.PnL = uplRatio * 100 // persen, sama persis dengan tampilan OKX
		coin.Leverage = state.Config.Leverage
		if markPx <= 0 {
			markPx = coin.Price // fallback harga WS bila mark belum tersedia
		}
		state.mu.Unlock()

		// addLog mengunci state.mu sendiri, jadi HARUS dipanggil setelah Unlock.
		if isOrphan {
			addLog(fmt.Sprintf("[WARN] Posisi terbuka di OKX untuk %s yang tidak ada di watchlist: %s %.4f kontrak", instId, side, info.Size))
		}

		// Watchdog: pastikan SETIAP posisi di OKX selalu punya TP+SL 100%.
		// ensureTPSL hanya melakukan 1 panggilan API (orders-algo-pending) dan
		// keluar tanpa log bila TP/SL sudah ada, sehingga aman dipanggil tiap sync.
		go ensureTPSL(instId, strings.ToLower(side), info.Size, avgPrice, snapshotTpSlConfig())

		// Trailing stop (F4): kelola SL dinamis bila konfigurasi aktif.
		// Memakai harga mark dari OKX (bukan WS) supaya konsisten dengan posisi.
		snapshotFull := func() Config {
			state.mu.RLock()
			defer state.mu.RUnlock()
			return state.Config
		}()
		if snapshotFull.TrailingEnabled {
			manageTrailing(instId, strings.ToLower(side), info.Size, avgPrice, markPx, snapshotFull)
		}
	}

	// Deteksi posisi yang baru saja tertutup oleh TP/SL OKX
	openPosMu.Lock()
	closed := make([]PositionInfo, 0)
	closedSymbol := make([]string, 0)
	for symbol, info := range openPositions {
		if _, stillOpen := live[symbol]; !stillOpen {
			closed = append(closed, info)
			closedSymbol = append(closedSymbol, symbol)
			delete(openPositions, symbol)
		}
	}
	for symbol, info := range live {
		openPositions[symbol] = info
	}
	openPosMu.Unlock()

	// Tandai koin tanpa posisi di OKX sebagai flat
	state.mu.Lock()
	for symbol, coin := range state.Coins {
		if _, isOpen := live[symbol]; !isOpen && coin.Position != "NONE" {
			coin.Position = "NONE"
			coin.Contracts = 0
			coin.EntryPrice = 0
			coin.HasTPSL = false
		}
	}
	state.mu.Unlock()

	// Hitung PnL riil dari OKX untuk posisi yang baru tertutup
	for i, symbol := range closedSymbol {
		info := closed[i]
		pnlPct, pnlUSDT := realizedPnL(symbol, info)
		state.mu.Lock()
		coin := state.Coins[symbol]
		showUSDT := false
		if coin != nil {
			coin.Position = "NONE"
			coin.Contracts = 0
			coin.PnL = pnlPct
			coin.HasTPSL = false
			showUSDT = true
		}
		state.TotalPnL += pnlUSDT
		total := state.TotalPnL
		state.mu.Unlock()

		if showUSDT {
			addLog(fmt.Sprintf("[EXIT] %s %s ditutup oleh OKX | %.4f kontrak | PnL: %+.2f%% (%+.4f USDT) | Total PnL: %+.4f USDT",
				info.Side, symbol, info.Size, pnlPct, pnlUSDT, total))
		}
	}

	if len(closedSymbol) == 0 {
		addLog("[SYNC] Positions and PnL synced with OKX")
	}
}

// realizedPnL mengambil PnL riil dari riwayat fill OKX (billing-archive) sehingga
// angka di dashboard identik dengan yang ditampilkan OKX.
func realizedPnL(symbol string, info PositionInfo) (float64, float64) {
	path := fmt.Sprintf("/api/v5/trade/fills?instId=%s&ordType=&limit=20", symbol)
	result, err := okxRequest("GET", path, "")
	if err == nil {
		if data, ok := result["data"].([]interface{}); ok {
			for _, item := range data {
				fill, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				side, _ := fill["side"].(string)
				posSide, _ := fill["posSide"].(string)
				accFillSzStr, _ := fill["accFillSz"].(string)
				avgPxStr, _ := fill["avgPx"].(string)
				feeStr, _ := fill["fee"].(string)
				feeCcy, _ := fill["feeCcy"].(string)

				accFillSz, _ := strconv.ParseFloat(accFillSzStr, 64)
				avgPx, _ := strconv.ParseFloat(avgPxStr, 64)
				fee, _ := strconv.ParseFloat(feeStr, 64)

				isExit := (posSide == "long" && side == "sell") || (posSide == "short" && side == "buy")
				if !isExit || accFillSz <= 0 {
					continue
				}
				// Hanya hitung fill yang keluar dari posisi ini
				if accFillSz < info.Size*0.5 {
					continue
				}

				ctVal := 1.0
				if spec, ok := getInstrumentSpec(symbol); ok {
					ctVal = spec.CtVal
				}
				var pnlPct float64
				if posSide == "short" {
					pnlPct = (info.EntryPrice - avgPx) / info.EntryPrice * 100
				} else {
					pnlPct = (avgPx - info.EntryPrice) / info.EntryPrice * 100
				}
				pnlUSDT := (avgPx - info.EntryPrice) * accFillSz * ctVal
				if posSide == "short" {
					pnlUSDT = -pnlUSDT
				}
				if feeCcy == "USDT" {
					pnlUSDT -= fee
				}
				return pnlPct, pnlUSDT
			}
		}
	}

	// Fallback: hitung dari harga trigger
	lastPrice := 0.0
	state.mu.RLock()
	if coin := state.Coins[symbol]; coin != nil {
		lastPrice = coin.Price
	}
	state.mu.RUnlock()
	ctVal := 1.0
	if spec, ok := getInstrumentSpec(symbol); ok {
		ctVal = spec.CtVal
	}
	pnlPct := (lastPrice - info.EntryPrice) / info.EntryPrice * 100
	if info.Side == "SHORT" {
		pnlPct = -pnlPct
	}
	pnlUSDT := pnlPct / 100 * info.Size * info.EntryPrice * ctVal
	return pnlPct, pnlUSDT
}

func getOpenPositionCount() int {
	state.mu.RLock()
	defer state.mu.RUnlock()
	count := 0
	for _, coin := range state.Coins {
		if coin.Symbol != "none" && coin.Position != "NONE" {
			count++
		}
	}
	return count
}

// ==========================================
// 3. WEBSOCKET & INDICATOR ENGINE
// ==========================================

var wsConnections = make(map[string]*websocket.Conn)
var candleData = make(map[string][]float64)

// CandleOHLC menyimpan OHLC satu candle — dibutuhkan untuk menghitung ATR
// (indikator ini butuh high, low, close, bukan hanya close).
type CandleOHLC struct {
	Ts               int64 // unix millisecond — dipakai mencegah candle ganda
	High, Low, Close float64
}

// candleOHLC menyimpan maksimal 100 candle terakhir per simbol. Ditulis di
// tempat yang sama dengan candleData (dalam kuncian state.mu).
var candleOHLC = make(map[string][]CandleOHLC)

// resetCandleHistory menghapus riwayat candle sebuah simbol.
// PEMANGGIL WAJIB memegang state.mu (write lock).
func resetCandleHistory(symbol string) {
	delete(candleData, symbol)
	delete(candleOHLC, symbol)
}

// ensureCandleHistory memastikan buffer candle sebuah simbol sudah ada.
// PEMANGGIL WAJIB memegang state.mu (write lock).
func ensureCandleHistory(symbol string) {
	if _, ok := candleData[symbol]; !ok {
		candleData[symbol] = make([]float64, 0, 100)
	}
	if _, ok := candleOHLC[symbol]; !ok {
		candleOHLC[symbol] = make([]CandleOHLC, 0, 100)
	}
}

// shouldTrackSymbol mengembalikan true jika simbol masih ada di watchlist ATAU
// punya posisi terbuka di OKX. Tanpa fungsi ini, goroutine reconnect WebSocket
// untuk koin yang sudah dibatalkan user akan terus berjalan selamanya dan
// ktetikus kembali menambahkan koin tersebut ke state.Coins.
func shouldTrackSymbol(symbol string) bool {
	state.mu.RLock()
	defer state.mu.RUnlock()
	for _, sym := range state.Config.Coins {
		if sym == symbol {
			return true
		}
	}
	if coin := state.Coins[symbol]; coin != nil && coin.Position != "NONE" {
		return true // masih ada posisi terbuka, tetap pantau
	}
	return false
}

func startWebSocket(symbol string) {
	// Jangan mulai (lalu reconnect tanpa henti) untuk koin yang tidak lagi dipilih.
	if !shouldTrackSymbol(symbol) {
		return
	}
	state.mu.RLock()
	mode := state.Config.Mode
	state.mu.RUnlock()
	url := "wss://ws.okx.com:8443/ws/v5/business"
	if mode == "demo" {
		url = "wss://wspap.okx.com:8443/ws/v5/business?brokerId=9999"
	}

	header := make(http.Header)
	header.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	c, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		addLog(fmt.Sprintf("[WS DIAL ERROR] %s: %v", symbol, err))
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(3 * time.Second)
		go startWebSocket(symbol)
		return
	}

	state.mu.Lock()
	// Close existing connection if any
	if oldConn, exists := wsConnections[symbol]; exists {
		oldConn.Close()
	}
	wsConnections[symbol] = c
	state.mu.Unlock()

	state.mu.RLock()
	timeframe := state.Config.Timeframe
	state.mu.RUnlock()
	subMsg := map[string]interface{}{
		"op": "subscribe",
		"args": []map[string]string{
			{"channel": "candle" + timeframe, "instId": symbol},
		},
	}
	msgBytes, _ := json.Marshal(subMsg)

	err = c.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		addLog(fmt.Sprintf("[WS SUBSCRIBE ERROR] %s: %v", symbol, err))
		c.Close()
		time.Sleep(3 * time.Second)
		go startWebSocket(symbol)
		return
	}

	addLog(fmt.Sprintf("[WS CONNECTED] %s berhasil terhubung.", symbol))

	go func() {
		done := make(chan struct{})
		var writeMu sync.Mutex
		go func() {
			ticker := time.NewTicker(20 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					writeMu.Lock()
					err := c.WriteMessage(websocket.TextMessage, []byte("ping"))
					writeMu.Unlock()
					if err != nil {
						c.Close()
						return
					}
				}
			}
		}()
		defer close(done)

		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				c.Close()
				state.mu.Lock()
				delete(wsConnections, symbol)
				state.mu.Unlock()
				// Berhenti total bila koin sudah dibatalkan dari watchlist.
				if !shouldTrackSymbol(symbol) {
					state.mu.Lock()
					if coin := state.Coins[symbol]; coin != nil && coin.Position == "NONE" {
						delete(state.Coins, symbol)
						resetCandleHistory(symbol)
					}
					state.mu.Unlock()
					addLog(fmt.Sprintf("[WS CLOSED] %s dihentikan (dihapus dari watchlist)", symbol))
					return
				}
				addLog(fmt.Sprintf("[WS READ ERROR] %s: %v. Reconnecting...", symbol, err))
				time.Sleep(3 * time.Second)
				go startWebSocket(symbol)
				return
			}

			msgStr := string(message)
			if msgStr == "ping" || msgStr == `{"event":"ping"}` {
				writeMu.Lock()
				err := c.WriteMessage(websocket.TextMessage, []byte("pong"))
				writeMu.Unlock()
				if err != nil {
					c.Close()
				}
				continue
			}

			var wsResp map[string]interface{}
			if err := json.Unmarshal(message, &wsResp); err != nil {
				continue
			}
			if event, ok := wsResp["event"].(string); ok {
				switch event {
				case "subscribe":
					addLog(fmt.Sprintf("[WS SUBSCRIBED] %s candle%s", symbol, timeframe))
					state.mu.Lock()
					if coin := state.Coins[symbol]; coin != nil {
						coin.WsError = ""
					}
					state.mu.Unlock()
				case "error":
					msg, _ := wsResp["msg"].(string)
					addLog(fmt.Sprintf("[WS SUBSCRIBE ERROR] %s: %v", symbol, msg))
					state.mu.Lock()
					if coin := state.Coins[symbol]; coin != nil {
						coin.WsError = msg
					}
					state.mu.Unlock()
				}
				continue
			}

			if data, ok := wsResp["data"].([]interface{}); ok && len(data) > 0 {
				if candleArr, ok := data[0].([]interface{}); ok && len(candleArr) >= 5 {
					// Format candle OKX: [ts, open, high, low, close, vol, ...]
					tsStr, ok0 := candleArr[0].(string)
					closePriceStr, ok := candleArr[4].(string)
					highStr, ok2 := candleArr[2].(string)
					lowStr, ok3 := candleArr[3].(string)
					if !ok0 || !ok || !ok2 || !ok3 {
						addLog(fmt.Sprintf("[WS DATA ERROR] %s: candle OHLC bukan string", symbol))
						continue
					}
					candleTs, terr := strconv.ParseInt(tsStr, 10, 64)
					closePrice, err := strconv.ParseFloat(closePriceStr, 64)
					highPrice, err2 := strconv.ParseFloat(highStr, 64)
					lowPrice, err3 := strconv.ParseFloat(lowStr, 64)
					if terr != nil || err != nil || err2 != nil || err3 != nil || closePrice <= 0 || highPrice < lowPrice || lowPrice <= 0 {
						addLog(fmt.Sprintf("[WS DATA ERROR] %s: invalid candle OHLC (h=%s l=%s c=%s)", symbol, highStr, lowStr, closePriceStr))
						continue
					}

					state.mu.Lock()
					if state.Coins[symbol] == nil {
						state.Coins[symbol] = &CoinState{Symbol: symbol, Position: "NONE"}
					}
					firstPrice := state.Coins[symbol].Price == 0
					state.Coins[symbol].Price = closePrice

					candleData[symbol] = append(candleData[symbol], closePrice)
					if len(candleData[symbol]) > 100 {
						candleData[symbol] = candleData[symbol][1:]
					}
					// Riwayat OHLC khusus ATR, disimpan SEKALI per candle (dedupe
					// berdasarkan timestamp — WS mengirim candle yang sama berulang
					// kali selama 5 menit saat harganya berubah).
					if l := len(candleOHLC[symbol]); l == 0 || candleOHLC[symbol][l-1].Ts != candleTs {
						candleOHLC[symbol] = append(candleOHLC[symbol], CandleOHLC{Ts: candleTs, High: highPrice, Low: lowPrice, Close: closePrice})
						if len(candleOHLC[symbol]) > 100 {
							candleOHLC[symbol] = candleOHLC[symbol][1:]
						}
					}

					isRunning := state.Config.IsRunning
					state.mu.Unlock()
					if firstPrice {
						addLog(fmt.Sprintf("[WS PRICE] %s: %.8f", symbol, closePrice))
					}

					if isRunning {
						checkTradingSignal(symbol, closePrice)
					}
				}
			}
		}
	}()
}

func checkTradingSignal(symbol string, currentPrice float64) {
	state.mu.Lock()
	cs := state.Coins[symbol]
	if cs == nil {
		state.mu.Unlock()
		return
	}
	if cs.Symbol == "none" || cs.Symbol == "" {
		state.mu.Unlock()
		return
	}

	// Snapshot konfigurasi TANPA lock ditahan saat request dikirim ke OKX.
	// Nilai inilah yang dipakai untuk order, jadi UI dan order selalu identik.
	balance := state.Balance
	cfgSnapshot := state.Config
	margin := effectiveMargin(cfgSnapshot, balance)
	leverage := cfgSnapshot.Leverage
	isRunning := cfgSnapshot.IsRunning
	if !isRunning {
		state.mu.Unlock()
		return
	}

	// Time filter (F3): posisi BARU hanya boleh dibuka di dalam jendela sesi
	// (UTC). Posisi terbuka TIDAK terpengaruh — tetap dikelola di luar jendela.
	if !timeFilterActive(cfgSnapshot) {
		state.mu.Unlock()
		return
	}

	// Hanya koin yang masih ada di watchlist yang boleh entry.
	inWatchlist := false
	for _, sym := range cfgSnapshot.Coins {
		if sym == symbol {
			inWatchlist = true
			break
		}
	}
	if !inWatchlist {
		state.mu.Unlock()
		return
	}

	// Snapshot TP/SL dari konfigurasi. Nilai ini yang dipakai untuk order,
	// sehingga tidak mungkin berbeda dari yang tampil di dashboard.
	tpPct := cfgSnapshot.TakeProfitPct
	slPct := cfgSnapshot.StopLossPct
	if tpPct <= 0 || slPct <= 0 {
		state.mu.Unlock()
		return
	}
	tpMode := cfgSnapshot.TpSlMode
	if tpMode != tpSlModeATR {
		tpMode = tpSlModePercent
	}
	atrPeriod := cfgSnapshot.AtrPeriod
	atrSlMult := cfgSnapshot.AtrSlMult

	// Perbarui pratinjau ukuran order di UI
	plan := calculateOrderSize(symbol, currentPrice, margin, leverage)
	applyOrderPlan(cs, plan, leverage)

	prices := append([]float64(nil), candleData[symbol]...)
	ohlc := append([]CandleOHLC(nil), candleOHLC[symbol]...)
	position := cs.Position
	lastSignalTime := cs.LastSignalTime
	state.mu.Unlock()

	if position != "NONE" || len(prices) < 10 {
		// TP/SL sekarang ditangani langsung oleh OKX (attachAlgoOrds / order-algo),
		// jadi bot TIDAK lagi menutup posisi manual. Ini mencegah partial close
		// dan mencegah exit ganda ketika order TP OKX sudah dieksekusi.
		return
	}

	// Ukuran order tidak valid (margin terlalu kecil untuk koin ini)
	if !plan.Valid {
		return
	}

	// Anti-spam: skip if last signal was less than 5 minutes ago
	if time.Since(lastSignalTime) < 5*time.Minute {
		return
	}

	_, upperBand, lowerBand := indicator.BollingerBands(prices)

	rsi, _ := indicator.Rsi(prices)
	currentRSI := rsi[len(rsi)-1]

	buySignal := currentPrice <= lowerBand[len(lowerBand)-1]*1.001 && currentRSI < 30
	sellSignal := currentPrice >= upperBand[len(upperBand)-1]*0.999 && currentRSI > 70
	if !buySignal && !sellSignal {
		return
	}

	if getOpenPositionCount() >= 5 {
		return // Max 5 concurrent trades reached
	}

	side, posSide, signalLabel := "sell", "short", "SELL SIGNAL (BB Upper + RSI>70)"
	if buySignal {
		side, posSide, signalLabel = "buy", "long", "BUY SIGNAL (BB Lower + RSI<30)"
	}

	state.mu.Lock()
	cs.Signal = signalLabel
	cs.LastSignalTime = time.Now()
	state.mu.Unlock()

	addLog(fmt.Sprintf("[SIGNAL] %s: %s | Price: %.8f | RSI: %.2f | Size: %.4f kontrak | Margin: $%.2f (target $%.2f) | %dx",
		symbol, strings.ToUpper(side), currentPrice, currentRSI, plan.Size, plan.MarginUsed, margin, leverage))

	if err := setLeverage(symbol, leverage); err != nil {
		addLog(fmt.Sprintf("[WARN] Gagal set leverage %s: %v", symbol, err))
	}

	spec, ok := getInstrumentSpec(symbol)
	if !ok {
		addLog(fmt.Sprintf("[ERROR] Spesifikasi %s belum tersedia, order dibatalkan", symbol))
		return
	}

	// Hitung harga TP/SL lewat SATU fungsi yang sama dengan watchdog & reprice.
	cfg := TpSlConfig{Mode: tpMode, TpPct: tpPct, SlPct: slPct, AtrPeriod: atrPeriod, AtrSlMult: atrSlMult, Leverage: leverage}
	calc := computeTpSlByMode(cfg, side, currentPrice, spec, ohlc)

	res, err := placeOrder(symbol, side, plan.Size, currentPrice, calc.TpPx, calc.SlPx)
	if err != nil {
		addLog(fmt.Sprintf("[ERROR] Order %s %s gagal: %v", symbol, side, err))
		return
	}

	state.mu.Lock()
	cs.Position = "LONG"
	if side == "sell" {
		cs.Position = "SHORT"
	}
	cs.EntryPrice = currentPrice
	cs.Contracts = plan.Size
	cs.TpPrice = calc.TpPx
	cs.SlPrice = calc.SlPx
	cs.HasTPSL = true
	cs.TpDistPct = calc.TpDistPct
	cs.SlDistPct = calc.SlDistPct
	cs.TpSlMode = calc.Mode
	cs.AtrUsed = calc.AtrVal
	state.mu.Unlock()

	modeTag := strings.ToUpper(calc.Mode)
	if calc.UsedPercent {
		modeTag = "PERSEN (fallback, ATR belum cukup)"
	}
	addLog(fmt.Sprintf("[EXECUTED] %s %s [%s] | ordId: %s | %.4f kontrak | Notional $%.2f | Margin terpakai $%.2f | %dx | TP: %s (+%.2f%%) | SL: %s (-%.2f%%) | R:R 1:%.2f | 100%% posisi",
		strings.ToUpper(posSide), symbol, modeTag, res.OrderID, plan.Size, plan.Notional, plan.MarginUsed, leverage,
		formatPx(calc.TpPx, spec), calc.TpDistPct, formatPx(calc.SlPx, spec), calc.SlDistPct, calc.TpDistPct/calc.SlDistPct))

	// Pastikan TP/SL benar-benar ada di OKX untuk 100% posisi
	go ensureTPSL(symbol, posSide, plan.Size, currentPrice, cfg)

	// Sinkronkan posisi & PnL aktual dari OKX
	go syncPositionsWithOKX()
}

// ==========================================
// 4. UTILITIES & STATE MANAGEMENT
// ==========================================

// OrderPlan adalah hasil perhitungan ukuran order yang BENAR-BENAR akan dikirim ke OKX.
type OrderPlan struct {
	Size         float64 // jumlah kontrak (kelipatan lotSz)
	Notional     float64 // nilai posisi dalam USDT
	MarginUsed   float64 // margin USDT yang benar-benar dipakai
	MinMarginReq float64 // margin minimum agar 1 lot bisa diorder
	Valid        bool
	Reason       string
}

// calculateOrderSize menghitung ukuran order dari spesifikasi kontrak resmi OKX.
//
// Rumus:  notional = margin x leverage
//
//	size     = floor(notional / (harga x ctVal) / lotSz) x lotSz
//
// Karena size dibulatkan ke bawah ke kelipatan lotSz, margin terpakai bisa sedikit di
// bawah input. Nilai MarginUsed dikembalikan agar UI menampilkan angka yang sama persis
// dengan yang dikirim ke OKX.
func calculateOrderSize(symbol string, price, margin float64, leverage int) OrderPlan {
	var plan OrderPlan
	if leverage <= 0 {
		plan.Reason = "leverage tidak valid"
		return plan
	}
	if margin <= 0 {
		plan.Reason = "margin harus lebih dari 0"
		return plan
	}

	spec, ok := getInstrumentSpec(symbol)
	if !ok {
		plan.Reason = "spesifikasi instrumen belum dimuat"
		return plan
	}
	if price <= 0 {
		plan.Reason = "harga belum tersedia"
		return plan
	}

	// Nilai 1 kontrak dalam USDT (ctVal = jumlah base coin per kontrak)
	valuePerContract := price * spec.CtVal

	// Margin minimum agar 1 lot bisa diorder
	plan.MinMarginReq = valuePerContract * spec.MinSz / float64(leverage)

	targetNotional := margin * float64(leverage)
	rawContracts := targetNotional / valuePerContract

	// Bulatkan ke bawah ke kelipatan lotSize yang sah
	size := math.Floor(rawContracts/spec.LotSz) * spec.LotSz
	// Hilangkan galat floating point
	size = math.Round(size/spec.LotSz) * spec.LotSz
	if size < spec.MinSz {
		plan.Reason = fmt.Sprintf("margin $%.2f @ %dx < min $%.2f", margin, leverage, plan.MinMarginReq)
		return plan
	}

	plan.Size = size
	plan.Notional = size * valuePerContract
	plan.MarginUsed = plan.Notional / float64(leverage)
	plan.Valid = true
	return plan
}

// applyOrderPlan menyimpan hasil perhitungan ke CoinState untuk ditampilkan di UI
func applyOrderPlan(coin *CoinState, plan OrderPlan, leverage int) {
	coin.OrderSize = plan.Size
	coin.Notional = plan.Notional
	coin.ActualMargin = plan.MarginUsed
	coin.MinMarginReq = plan.MinMarginReq
	coin.IsValidSize = plan.Valid
	coin.Leverage = leverage
}

func addLog(msg string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	timestamp := time.Now().Format("15:04:05")
	logMsg := fmt.Sprintf("[%s] %s", timestamp, msg)
	state.Logs = append([]string{logMsg}, state.Logs...)
	if len(state.Logs) > 50 {
		state.Logs = state.Logs[:50]
	}
	// Also print to stdout for debugging
	fmt.Println(logMsg)
}

// ==========================================
// 5. HTTP HANDLERS
// ==========================================

func handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.New("web").Parse(uiTemplate)
	tmpl.Execute(w, nil)
}

func handleGetState(w http.ResponseWriter, r *http.Request) {
	state.mu.Lock()

	// Remove coins that are not in config, TETAPI jangan hapus yang masih punya
	// posisi terbuka di OKX (dibutuhkan untuk counting & tampilan dashboard).
	for sym, coin := range state.Coins {
		isInConfig := false
		for _, configSym := range state.Config.Coins {
			if sym == configSym {
				isInConfig = true
				break
			}
		}
		if !isInConfig && coin.Position == "NONE" {
			delete(state.Coins, sym)
			resetCandleHistory(sym)
		}
	}

	newSymbols := make([]string, 0)

	for _, sym := range state.Config.Coins {
		if sym == "none" || sym == "" {
			continue
		}
		if _, exists := state.Coins[sym]; !exists {
			state.Coins[sym] = &CoinState{Symbol: sym, Position: "NONE"}
			newSymbols = append(newSymbols, sym)
		}
		// Pratinjau order dihitung dengan harga TERKINI sehingga angka di UI
		// sama persis dengan yang akan dikirim ke OKX. Margin memakai
		// effectiveMargin (F1) bila position sizing aktif.
		coin := state.Coins[sym]
		effMargin := effectiveMargin(state.Config, state.Balance)
		plan := calculateOrderSize(sym, coin.Price, effMargin, state.Config.Leverage)
		applyOrderPlan(coin, plan, state.Config.Leverage)
	}

	response := struct {
		Config       Config
		Coins        map[string]*CoinState
		Logs         []string
		TotalPnL     float64
		StartTime    time.Time
		Balance      float64 `json:"balance"`      // total equity USDT (F1/F2 basis)
		DailyPnL     float64 `json:"dailyPnL"`     // PnL harian sejak 00:00 UTC
		LossLimitHit bool    `json:"lossLimitHit"` // engine terkunci karena loss harian
		LossDay      string  `json:"lossDay"`      // tanggal UTC dari penghitungan harian
	}{Config: state.Config, Coins: make(map[string]*CoinState, len(state.Coins)), Logs: append([]string(nil), state.Logs...), TotalPnL: state.TotalPnL, StartTime: state.StartTime, Balance: state.Balance, DailyPnL: state.DailyPnL, LossLimitHit: state.LossLimitHit, LossDay: state.LossDay}
	for symbol, coin := range state.Coins {
		coinCopy := *coin
		response.Coins[symbol] = &coinCopy
	}
	state.mu.Unlock()

	for _, sym := range newSymbols {
		go startWebSocket(sym)
		go seedCandleHistory(sym)
	}
	json.NewEncoder(w).Encode(response)
}

func handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var newConfig Config
	if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// Validate inputs
	if newConfig.MarginUSDT <= 0 {
		http.Error(w, "margin must be positive", 400)
		return
	}
	if newConfig.Leverage < 1 || newConfig.Leverage > 125 {
		http.Error(w, "leverage must be between 1 and 125", 400)
		return
	}

	// --- Validasi TP/SL -------------------------------------------------
	// Default ke rasio 1:2 yang benar bila field tidak dikirim.
	if newConfig.TakeProfitPct == 0 && newConfig.StopLossPct == 0 {
		newConfig.TakeProfitPct = defaultTakeProfitPct
		newConfig.StopLossPct = defaultStopLossPct
	}
	if newConfig.TakeProfitPct <= 0 || newConfig.StopLossPct <= 0 {
		http.Error(w, "take profit and stop loss must both be greater than 0", 400)
		return
	}
	if newConfig.TakeProfitPct > 50 || newConfig.StopLossPct > 50 {
		http.Error(w, "take profit and stop loss must be at most 50%", 400)
		return
	}
	// Rasio risiko:hadiah harus >= 1: TP HARUS >= SL. Jika TP lebih kecil dari
	// SL, risiko lebih besar dari hadiah sehingga bot butuh win rate sangat
	// tinggi untuk impas. Tolong perbaiki di UI.
	if newConfig.RiskReward() < 1 {
		http.Error(w, fmt.Sprintf(
			"take profit (%.2f%%) must be greater than or equal to stop loss (%.2f%%) - "+
				"otherwise you risk more than you gain (ratio 1:%.2f, break-even win rate %.1f%%)",
			newConfig.TakeProfitPct, newConfig.StopLossPct, newConfig.RiskReward(), newConfig.BreakEvenWinRate()), 400)
		return
	}

	// --- Validasi metode penempatan TP/SL (persen / ATR) -----------------
	if newConfig.TpSlMode == "" {
		newConfig.TpSlMode = tpSlModeATR
	}
	if newConfig.TpSlMode != tpSlModePercent && newConfig.TpSlMode != tpSlModeATR {
		http.Error(w, fmt.Sprintf("tpSlMode must be %q or %q", tpSlModePercent, tpSlModeATR), 400)
		return
	}
	if newConfig.AtrPeriod <= 0 {
		newConfig.AtrPeriod = defaultAtrPeriod
	}
	if newConfig.AtrPeriod < 2 || newConfig.AtrPeriod > 200 {
		http.Error(w, "atrPeriod must be between 2 and 200", 400)
		return
	}
	if newConfig.AtrSlMult <= 0 {
		newConfig.AtrSlMult = defaultAtrSlMult
	}
	if newConfig.AtrSlMult > 5 {
		http.Error(w, "atrSlMult must be at most 5", 400)
		return
	}

	// --- Validasi F1: Position Sizing -----------------------------------
	if newConfig.PosSizingMode == "" {
		newConfig.PosSizingMode = "pct"
	}
	if newConfig.PosSizingMode != "pct" && newConfig.PosSizingMode != "fixed" {
		http.Error(w, "posSizingMode must be \"pct\" or \"fixed\"", 400)
		return
	}
	if newConfig.PosSizingEnabled {
		if newConfig.PosSizingMode == "pct" {
			if newConfig.PosSizingValue <= 0 || newConfig.PosSizingValue > 100 {
				http.Error(w, "position sizing % must be between 0 and 100", 400)
				return
			}
		} else {
			if newConfig.PosSizingValue <= 0 {
				http.Error(w, "fixed position sizing must be positive (USDT)", 400)
				return
			}
			// Nominal tidak boleh melebihi saldo akun (bila saldo sudah diketahui).
			state.mu.RLock()
			bal := state.Balance
			state.mu.RUnlock()
			if bal > 0 && newConfig.PosSizingValue > bal {
				http.Error(w, fmt.Sprintf("fixed position sizing $%.2f exceeds account balance $%.2f", newConfig.PosSizingValue, bal), 400)
				return
			}
		}
	}

	// --- Validasi F2: Daily Loss Limit ----------------------------------
	if newConfig.LossLimitMode == "" {
		newConfig.LossLimitMode = "pct"
	}
	if newConfig.LossLimitMode != "pct" && newConfig.LossLimitMode != "fixed" {
		http.Error(w, "lossLimitMode must be \"pct\" or \"fixed\"", 400)
		return
	}
	if newConfig.LossLimitEnabled {
		if newConfig.LossLimitValue <= 0 {
			http.Error(w, "loss limit value must be positive", 400)
			return
		}
		if newConfig.LossLimitMode == "pct" && newConfig.LossLimitValue > 100 {
			http.Error(w, "loss limit % must be at most 100", 400)
			return
		}
	}

	// --- Validasi F3: Time Filter ---------------------------------------
	if newConfig.TimeFilterMode == "" {
		newConfig.TimeFilterMode = timeFilter247
	}
	switch newConfig.TimeFilterMode {
	case timeFilter247, timeFilterAsian, timeFilterLondon, timeFilterNewYork, timeFilterOverlap:
		// jendela bawaan, valid
	case timeFilterCustom:
		if newConfig.CustomStartHour < 0 || newConfig.CustomStartHour > 23 ||
			newConfig.CustomEndHour < 0 || newConfig.CustomEndHour > 23 {
			http.Error(w, "custom session hours must be between 0 and 23 (UTC)", 400)
			return
		}
	default:
		http.Error(w, "unknown timeFilterMode", 400)
		return
	}

	// --- Validasi F4: Trailing Stop -------------------------------------
	if newConfig.TrailingEnabled {
		if newConfig.TrailingTriggerPct <= 0 {
			http.Error(w, "trailing trigger profit must be positive", 400)
			return
		}
		// Jarak trailing wajib > biaya round-trip OKX (~0.16%) agar stop-loss
		// tidak tersentuh hanya karena fee transaksi (PRD F4).
		if newConfig.TrailingDistPct < trailingMinDistPct {
			http.Error(w, fmt.Sprintf("trailing distance must be greater than %.2f%% (OKX round-trip fee ~0.16%%)", trailingMinDistPct), 400)
			return
		}
		if newConfig.TrailingDistPct > trailingMaxDistPct {
			http.Error(w, "trailing distance must be at most 20%", 400)
			return
		}
	}

	// --- Kunci restart bila batas loss harian tercapai (F2) -------------
	if newConfig.IsRunning {
		state.mu.Lock()
		ensureLossDayResetLocked()
		wasHit := state.LossLimitHit
		daily := state.DailyPnL
		bal := state.Balance
		state.mu.Unlock()
		if wasHit && newConfig.LossLimitEnabled {
			limit := dailyLossLimit(newConfig, bal)
			if limit > 0 && daily <= -limit {
				http.Error(w, fmt.Sprintf(
					"daily loss limit reached: PnL %.4f USDT vs limit -%.4f USDT. Engine locked until 00:00 UTC — naikkan/nonaktifkan batas untuk restart.",
					daily, limit), 400)
				return
			}
			// Batas dinaikkan / loss sudah pulih -> buka kunci.
			state.mu.Lock()
			state.LossLimitHit = false
			state.mu.Unlock()
			addLog("[LOSS LIMIT] Batas loss harian dinaikkan/dinonaktifkan; kunci engine dibuka.")
		}
	}

	// Build new coin list (filter out "none" and empty)
	//同时 saring instrumen yang tidak ada di OKX (delisted / typo) agar bot
	//tidak pernah mencoba order pada instrumen yang tidak valid.
	newCoins := make([]string, 0)
	seen := make(map[string]bool)
	rejected := make([]string, 0)
	for _, sym := range newConfig.Coins {
		if sym == "none" || sym == "" {
			continue
		}
		if seen[sym] {
			continue
		}
		if _, ok := getInstrumentSpec(sym); !ok {
			rejected = append(rejected, sym)
			continue
		}
		seen[sym] = true
		newCoins = append(newCoins, sym)
	}
	if len(rejected) > 0 {
		addLog(fmt.Sprintf("[CONFIG WARNING] Instrumen tidak tersedia di OKX, dilewati: %s", strings.Join(rejected, ", ")))
		http.Error(w, "instruments not available on OKX: "+strings.Join(rejected, ", "), http.StatusBadRequest)
		return
	}
	if len(newCoins) == 0 {
		http.Error(w, "pilih minimal satu instrumen", http.StatusBadRequest)
		return
	}
	// Jaga konfigurasi coins agar tidak mengandung entri kosong/"none" di tengah
	for i := len(newCoins); i < 5; i++ {
		newCoins = append(newCoins, "none")
	}

	state.mu.Lock()

	// Cek apakah konfigurasi TP/SL berubah, agar posisi terbuka bisa di-reprice
	// (mode, persen, periode/multiplier ATR — semuanya mempengaruhi harga level).
	tpslChanged := newConfig.TakeProfitPct != state.Config.TakeProfitPct ||
		newConfig.StopLossPct != state.Config.StopLossPct ||
		newConfig.TpSlMode != state.Config.TpSlMode ||
		newConfig.AtrPeriod != state.Config.AtrPeriod ||
		newConfig.AtrSlMult != state.Config.AtrSlMult

	// Update config
	state.Config.MarginUSDT = newConfig.MarginUSDT
	state.Config.Leverage = newConfig.Leverage
	state.Config.Coins = [5]string{}
	copy(state.Config.Coins[:], newCoins)
	state.Config.IsRunning = newConfig.IsRunning
	state.Config.Timeframe = newConfig.Timeframe
	state.Config.TakeProfitPct = newConfig.TakeProfitPct
	state.Config.StopLossPct = newConfig.StopLossPct
	state.Config.TpSlMode = newConfig.TpSlMode
	state.Config.AtrPeriod = newConfig.AtrPeriod
	state.Config.AtrSlMult = newConfig.AtrSlMult

	// --- F1: Position Sizing ---
	state.Config.PosSizingEnabled = newConfig.PosSizingEnabled
	state.Config.PosSizingMode = newConfig.PosSizingMode
	state.Config.PosSizingValue = newConfig.PosSizingValue

	// --- F2: Daily Loss Limit ---
	state.Config.LossLimitEnabled = newConfig.LossLimitEnabled
	state.Config.LossLimitMode = newConfig.LossLimitMode
	state.Config.LossLimitValue = newConfig.LossLimitValue

	// --- F3: Time Filter ---
	state.Config.TimeFilterMode = newConfig.TimeFilterMode
	state.Config.CustomStartHour = newConfig.CustomStartHour
	state.Config.CustomEndHour = newConfig.CustomEndHour

	// --- F4: Trailing Stop ---
	state.Config.TrailingEnabled = newConfig.TrailingEnabled
	state.Config.TrailingTriggerPct = newConfig.TrailingTriggerPct
	state.Config.TrailingDistPct = newConfig.TrailingDistPct

	// Tentukan simbol yang tidak lagi dipilih, TETAPI posisi yang masih terbuka
	// di OKX tetap dipertahankan agar PnL & TP/SL tidak hilang dari dashboard.
	stillTracked := make(map[string]bool, len(newCoins))
	for _, sym := range newCoins {
		stillTracked[sym] = true
	}
	for sym, coin := range state.Coins {
		if !stillTracked[sym] && coin.Position == "NONE" {
			delete(state.Coins, sym)
			resetCandleHistory(sym)
		}
	}
	// Tutup WebSocket hanya untuk koin yang tidak dipilih DAN tidak punya posisi
	for sym, conn := range wsConnections {
		if !stillTracked[sym] {
			coin := state.Coins[sym]
			if coin == nil || coin.Position == "NONE" {
				conn.Close()
				delete(wsConnections, sym)
			}
		}
	}

	// Tambahkan koin yang baru dipilih
	for _, sym := range newCoins {
		if _, exists := state.Coins[sym]; !exists {
			state.Coins[sym] = &CoinState{Symbol: sym, Position: "NONE"}
		}
		ensureCandleHistory(sym)
	}

	state.mu.Unlock()

	// Mulai WebSocket untuk koin yang dipilih
	for _, sym := range newCoins {
		go startWebSocket(sym)
		go seedCandleHistory(sym)
	}

	addLog(fmt.Sprintf("CONFIG: TP/SL mode %s | TP %.2f%% | SL %.2f%% | ATR %dx%.2f | Risk:Reward 1:%.2f | break-even win rate %.1f%%",
		strings.ToUpper(newConfig.TpSlMode), newConfig.TakeProfitPct, newConfig.StopLossPct,
		newConfig.AtrPeriod, newConfig.AtrSlMult, newConfig.RiskReward(), newConfig.BreakEvenWinRate()))

	// Ringkasan fitur risiko (F1-F4) di log, agar perilaku aktif selalu jelas.
	feat := make([]string, 0, 4)
	if newConfig.PosSizingEnabled {
		if newConfig.PosSizingMode == "fixed" {
			feat = append(feat, fmt.Sprintf("Size=%.2f USDT", newConfig.PosSizingValue))
		} else {
			feat = append(feat, fmt.Sprintf("Size=%.2f%% saldo", newConfig.PosSizingValue))
		}
	}
	if newConfig.LossLimitEnabled {
		if newConfig.LossLimitMode == "fixed" {
			feat = append(feat, fmt.Sprintf("Loss=%.2f USDT", newConfig.LossLimitValue))
		} else {
			feat = append(feat, fmt.Sprintf("Loss=%.2f%%", newConfig.LossLimitValue))
		}
	}
	feat = append(feat, fmt.Sprintf("Sesi=%s", strings.ToUpper(newConfig.TimeFilterMode)))
	if newConfig.TrailingEnabled {
		feat = append(feat, fmt.Sprintf("Trailing=%.2f%%/%.2f%%", newConfig.TrailingTriggerPct, newConfig.TrailingDistPct))
	}
	if len(feat) > 0 {
		addLog("CONFIG Risiko: " + strings.Join(feat, " | "))
	}

	// Posisi yang sudah terbuka memakai TP/SL lama. Supaya konfigurasi UI
	// selalu berlaku untuk semua posisi, harga TP/SL di-reprice sesuai
	// konfigurasi baru (persen atau ATR terkini).
	if tpslChanged {
		go repriceOpenPositionsTpSl(TpSlConfig{
			Mode:      newConfig.TpSlMode,
			TpPct:     newConfig.TakeProfitPct,
			SlPct:     newConfig.StopLossPct,
			AtrPeriod: newConfig.AtrPeriod,
			AtrSlMult: newConfig.AtrSlMult,
			Leverage:  newConfig.Leverage,
		})
	}

	if newConfig.IsRunning {
		state.mu.RLock()
		bal := state.Balance
		state.mu.RUnlock()
		effMargin := effectiveMargin(newConfig, bal)
		addLog(fmt.Sprintf("CONFIG: Bot STARTED | Margin $%.2f/trade (F1 %s) | %dx leverage | %d koin: %s",
			effMargin, f1modeLabel(newConfig), newConfig.Leverage, len(newCoins), strings.Join(newCoins, ", ")))
		state.mu.Lock()
		state.StartTime = time.Now()
		state.mu.Unlock()
		go syncPositionsWithOKX()
	} else {
		addLog("CONFIG: Bot STOPPED.")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

func handleEmergencyClose(w http.ResponseWriter, r *http.Request) {
	type positionToClose struct {
		symbol   string
		position string
	}
	state.mu.Lock()
	positions := make([]positionToClose, 0)
	for symbol, coin := range state.Coins {
		if coin.Position != "NONE" {
			positions = append(positions, positionToClose{symbol: symbol, position: coin.Position})
		}
	}
	state.Config.IsRunning = false
	state.mu.Unlock()

	failed := false
	for _, position := range positions {
		addLog(fmt.Sprintf("[EMERGENCY] Menutup 100%% posisi %s %s di OKX", position.position, position.symbol))
		// close-position menutup SELURUH posisi, tidak mungkin partial
		if err := closePosition(position.symbol, ""); err != nil {
			failed = true
			addLog(fmt.Sprintf("[EMERGENCY ERROR] Gagal menutup %s: %v", position.symbol, err))
			continue
		}
		openPosMu.Lock()
		delete(openPositions, position.symbol)
		openPosMu.Unlock()

		state.mu.Lock()
		if coin := state.Coins[position.symbol]; coin != nil {
			coin.Position = "NONE"
			coin.EntryPrice = 0
			coin.Contracts = 0
			coin.HasTPSL = false
		}
		state.mu.Unlock()
		addLog(fmt.Sprintf("[EMERGENCY SUCCESS] Posisi %s %s ditutup penuh", position.symbol, position.position))
	}

	// Batalkan semua order TP/SL yang masih menggantung agar tidak memicu order reversal
	cancelPendingAlgoOrders("EMERGENCY")

	// Close all WebSocket connections
	state.mu.Lock()
	for symbol, conn := range wsConnections {
		conn.Close()
		delete(wsConnections, symbol)
	}
	state.mu.Unlock()

	addLog("[EMERGENCY] Bot dihentikan secara paksa.")
	if failed {
		http.Error(w, "one or more positions could not be closed; check bot logs and OKX", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// cancelAlgoOrdersFor membatalkan order TP/SL conditional milik satu instrumen saja.
func cancelAlgoOrdersFor(symbol string) error {
	path := fmt.Sprintf("/api/v5/trade/orders-algo-pending?ordType=conditional&instType=SWAP&instId=%s", symbol)
	result, err := okxRequest("GET", path, "")
	if err != nil {
		return err
	}
	data, _ := result["data"].([]interface{})
	var lastErr error
	for _, item := range data {
		algo, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		algoId, _ := algo["algoId"].(string)
		instId, _ := algo["instId"].(string)
		if algoId == "" {
			continue
		}
		body, _ := json.Marshal(map[string]interface{}{"instId": instId, "algoId": algoId})
		if _, err := okxRequest("POST", "/api/v5/trade/cancel-algos", string(body)); err != nil {
			lastErr = err
			addLog(fmt.Sprintf("[WARN] Gagal batalkan algo %s: %v", algoId, err))
		}
	}
	return lastErr
}

// cancelPendingAlgoOrders membatalkan seluruh order TP/SL conditional yang menggantung.
// label dipakai di pesan log (mis. "EMERGENCY" atau "LOSS LIMIT").
func cancelPendingAlgoOrders(label string) {
	result, err := okxRequest("GET", "/api/v5/trade/orders-algo-pending?ordType=conditional&instType=SWAP", "")
	if err != nil {
		return
	}
	data, _ := result["data"].([]interface{})
	for _, item := range data {
		algo, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		algoId, _ := algo["algoId"].(string)
		instId, _ := algo["instId"].(string)
		if algoId == "" {
			continue
		}
		body, _ := json.Marshal(map[string]interface{}{"instId": instId, "algoId": algoId})
		if _, err := okxRequest("POST", "/api/v5/trade/cancel-algos", string(body)); err != nil {
			addLog(fmt.Sprintf("[WARN] Gagal batalkan algo %s: %v", algoId, err))
		}
	}
	addLog(fmt.Sprintf("[%s] Semua order TP/SL pending dibatalkan", label))
}

// ==========================================
// 6. MAIN ENTRY POINT
// ==========================================

func main() {
	if state.Config.Mode != "demo" && state.Config.Mode != "live" {
		log.Fatalf("invalid OKX_MODE %q: use demo or live", state.Config.Mode)
	}
	if state.Config.Mode == "live" {
		log.Println("WARNING: LIVE TRADING MODE. Orders will use real funds.")
	} else {
		log.Println("OKX demo trading mode enabled.")
	}
	addLog(fmt.Sprintf("[SYSTEM] Trading mode: %s", strings.ToUpper(state.Config.Mode)))

	// Muat spesifikasi kontrak resmi OKX (ctVal/lotSz/minSz/tickSz).
	// Wajib, tanpa ini margin & ukuran order tidak akan pernah sesuai input user.
	loadInstrumentSpecs()
	go func() {
		for {
			time.Sleep(6 * time.Hour)
			loadInstrumentSpecs()
		}
	}()

	for _, sym := range state.Config.Coins {
		if sym == "none" || sym == "" {
			continue
		}
		state.Coins[sym] = &CoinState{Symbol: sym, Position: "NONE"}
		ensureCandleHistory(sym)
		go startWebSocket(sym)
		go seedCandleHistory(sym)
	}

	// Sync positions with OKX on startup
	go syncPositionsWithOKX()
	// Muat data akun (saldo + PnL harian) sekali di awal untuk UI & F1/F2.
	refreshAccountData()

	// Sinkronisasi berkala: memastikan posisi, ukuran kontrak, dan PnL di dashboard
	// selalu sama dengan OKX, serta mendeteksi exit yang dieksekusi order TP/SL OKX.
	go func() {
		for {
			time.Sleep(10 * time.Second)
			// Saldo & PnL harian dipantau terus (dipakai F1/F2 dan tampilan UI),
			// termasuk saat engine berhenti — posisi terbuka tetap bisa rugi.
			refreshAccountData()
			state.mu.RLock()
			running := state.Config.IsRunning
			state.mu.RUnlock()
			if running {
				syncPositionsWithOKX()
			}
		}
	}()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/state", handleGetState)
	http.HandleFunc("/api/config", handleUpdateConfig)
	http.HandleFunc("/api/emergency", handleEmergencyClose)

	fmt.Println("========================================")
	fmt.Printf("  OKX SCALPER PRO (%s MODE)\n", strings.ToUpper(state.Config.Mode))
	fmt.Println("  UI Ready at: http://localhost:8080")
	fmt.Println("========================================")

	log.Fatal(http.ListenAndServe("0.0.0.0:8080", nil))
}

// ==========================================
// 7. UI TEMPLATE (API INPUTS REMOVED)
// ==========================================

const uiTemplate = `
<!DOCTYPE html>
<html lang="en" data-theme="light">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>OKX Algo-Trader Pro</title>
    <!-- Ikon web (SVG inline, tanpa file eksternal) + warna chrome browser mobile -->
    <link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'%20viewBox='0%200%2032%2032'%3E%3Cdefs%3E%3ClinearGradient%20id='g'%20x1='0'%20y1='0'%20x2='1'%20y2='1'%3E%3Cstop%20offset='0'%20stop-color='%230d9488'/%3E%3Cstop%20offset='1'%20stop-color='%2310b981'/%3E%3C/linearGradient%3E%3C/defs%3E%3Crect%20width='32'%20height='32'%20rx='8'%20fill='url(%23g)'/%3E%3Cpath%20d='M6%2021l6-7%205%204%208-9'%20fill='none'%20stroke='%23ffffff'%20stroke-width='2.4'%20stroke-linecap='round'%20stroke-linejoin='round'/%3E%3C/svg%3E">
    <link rel="apple-touch-icon" href="data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'%20viewBox='0%200%2032%2032'%3E%3Cdefs%3E%3ClinearGradient%20id='g'%20x1='0'%20y1='0'%20x2='1'%20y2='1'%3E%3Cstop%20offset='0'%20stop-color='%230d9488'/%3E%3Cstop%20offset='1'%20stop-color='%2310b981'/%3E%3C/linearGradient%3E%3C/defs%3E%3Crect%20width='32'%20height='32'%20rx='8'%20fill='url(%23g)'/%3E%3Cpath%20d='M6%2021l6-7%205%204%208-9'%20fill='none'%20stroke='%23ffffff'%20stroke-width='2.4'%20stroke-linecap='round'%20stroke-linejoin='round'/%3E%3C/svg%3E">
    <meta name="theme-color" content="#0d9488">
    <script>
        // WAJIB di baris pertama <head>: tema diterapkan sebelum CSS, font,
        // dan Alpine dimuat supaya tidak ada kedipan (FOUC) saat reload.
        // Default: light. Dark hanya dipakai jika user pernah memilihnya
        // atau OS memang dalam mode gelap dan belum ada preferensi tersimpan.
        (function () {
            var key = 'okx-ui-theme';
            var saved = null;
            try { saved = localStorage.getItem(key); } catch (e) {}
            var prefersDark = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
            document.documentElement.setAttribute('data-theme', saved || (prefersDark ? 'dark' : 'light'));
        })();
    </script>
    <script src="https://cdn.tailwindcss.com"></script>
    <script defer src="https://unpkg.com/alpinejs@3.x.x/dist/cdn.min.js"></script>
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@300;400;500;600;700;800&display=swap" rel="stylesheet">
    <style>
        /* ==========================================================
           1. DESIGN TOKENS
           Semua warna UI memakai variabel ini, jadi light/dark cukup
           menimpa nilai - tidak ada kelas warna yang hardcoded.
           ========================================================== */
        :root {
            /* --- Permukaan kaca ---------------------------------------
               Alpha SENGAJA RENDAH (0.28-0.55). KalauKINAIKAN di atas 0.6,
               warna background di belakang tidak terlihat lagi sehingga blur
               tidak terbaca dan kartu terasa seperti blok putih biasa.
               Gradient (bukan warna datar) memberi kesan kaca yang memantulkan
               cahaya dari sudut berbeda - inilah yang bikin "elegan". */
            --surface: linear-gradient(150deg, rgba(255, 255, 255, 0.60) 0%, rgba(255, 255, 255, 0.28) 46%, rgba(255, 255, 255, 0.46) 100%);
            --surface-bar: linear-gradient(180deg, rgba(255, 255, 255, 0.74) 0%, rgba(255, 255, 255, 0.40) 100%);
            --surface-2: rgba(255, 255, 255, 0.68);
            --pane: rgba(255, 255, 255, 0.44);
            --pane-sunken: rgba(15, 23, 42, 0.06);
            --pane-border: rgba(255, 255, 255, 0.88);
            --field-bg: rgba(255, 255, 255, 0.62);

            /* Rim kaca = putih semi-transparan (menangkap cahaya).
               Outline = garis gelap tipis. Keduanya wajib: rim memberi
               kesan kaca, outline yang MEMBEDAKAN kartu dari background. */
            --border: rgba(255, 255, 255, 0.72);
            --outline: rgba(15, 23, 42, 0.11);
            --outline-strong: rgba(15, 23, 42, 0.20);
            --border-strong: rgba(15, 23, 42, 0.16);
            --highlight: rgba(255, 255, 255, 0.95);

            --text: #0c1d2b;
            --text-muted: #44576a;
            --text-subtle: #56697b;
            --accent: #0d9488;
            --accent-2: #10b981;
            --accent-contrast: #ffffff;
            --positive: #047857;
            --negative: #e11d48;
            --warning: #b45309;
            /* Bayangan harus JELAS - inilah penanda kartu "melayang" di atas
               background, pengganti outline yang tidak bisa PAY off di glass. */
            --shadow-card: 0 1px 1px rgba(15, 23, 42, 0.05), 0 14px 32px -14px rgba(15, 23, 42, 0.34);
            --shadow-lift: 0 2px 4px rgba(15, 23, 42, 0.06), 0 30px 58px -26px rgba(13, 148, 136, 0.5);
            --ring: 0 0 0 4px rgba(13, 148, 136, 0.16);
        }

        [data-theme="dark"] {
            --surface: linear-gradient(150deg, rgba(30, 41, 59, 0.55) 0%, rgba(15, 23, 42, 0.30) 46%, rgba(30, 41, 59, 0.50) 100%);
            --surface-bar: linear-gradient(180deg, rgba(30, 41, 59, 0.68) 0%, rgba(15, 23, 42, 0.38) 100%);
            --surface-2: rgba(30, 41, 59, 0.62);
            --pane: rgba(2, 6, 23, 0.30);
            --pane-sunken: rgba(2, 6, 23, 0.45);
            --pane-border: rgba(255, 255, 255, 0.10);
            --field-bg: rgba(2, 6, 23, 0.38);
            --border: rgba(255, 255, 255, 0.13);
            --outline: rgba(255, 255, 255, 0.09);
            --outline-strong: rgba(255, 255, 255, 0.20);
            --border-strong: rgba(148, 163, 184, 0.34);
            --highlight: rgba(255, 255, 255, 0.14);
            --text: #e7eef6;
            --text-muted: #9aa9bb;
            --text-subtle: #7c8ea1;
            --accent: #2dd4bf;
            --accent-2: #34d399;
            --accent-contrast: #04231f;
            --positive: #34d399;
            --negative: #fb7185;
            --warning: #fbbf24;
            --shadow-card: 0 1px 1px rgba(0, 0, 0, 0.4), 0 18px 40px -20px rgba(0, 0, 0, 0.85);
            --shadow-lift: 0 2px 4px rgba(0, 0, 0, 0.4), 0 32px 64px -26px rgba(45, 212, 191, 0.4);
            --ring: 0 0 0 4px rgba(45, 212, 191, 0.18);
        }

        /* ==========================================================
           2. BASE
           ========================================================== */
        * { -webkit-tap-highlight-color: transparent; }
        html { scroll-behavior: smooth; }
        body {
            font-family: 'Plus Jakarta Sans', ui-sans-serif, system-ui, sans-serif;
            color: var(--text);
            background: #dfe9f2;
            overflow-x: hidden;
            -webkit-font-smoothing: antialiased;
        }
        [data-theme="dark"] body { background: #0b1425; }
        .mono { font-family: ui-monospace, 'JetBrains Mono', SFMono-Regular, Menlo, monospace; font-variant-numeric: tabular-nums; }
        [x-cloak] { display: none !important; }

        /* ==========================================================
           3. BACKGROUND - glass mesh statis + drift sangat lambat
           ========================================================== */
        .canvas {
            position: fixed;
            inset: 0;
            z-index: -1;
            overflow: hidden;
            /* Base DIBUAT lebih gelap dari kartu kaca. Kalau base ~= warna kartu,
               kartu jadi tidakWU Entah tidak terlihat karena tidak ada yang "melewati".
               Kaca selalu butuh bagian yang terang dan bagian yang gelap di sekitarnya. */
            background: linear-gradient(165deg, #dfe9f2 0%, #d7e5ee 42%, #d9ebe7 78%, #cfe4de 100%);
        }
        [data-theme="dark"] .canvas {
            background: linear-gradient(165deg, #0b1425 0%, #08101d 45%, #07161a 80%, #06121a 100%);
        }
        .blob {
            position: absolute;
            border-radius: 9999px;
            filter: blur(80px);
            will-change: transform;
            opacity: 0.75;
        }
        .blob-1 { width: 48rem; height: 48rem; top: -16rem; left: -13rem; background: radial-gradient(circle, rgba(13, 148, 136, 0.55), transparent 66%); animation: drift-a 34s ease-in-out infinite; }
        .blob-2 { width: 42rem; height: 42rem; top: -8rem; right: -12rem; background: radial-gradient(circle, rgba(16, 185, 129, 0.48), transparent 66%); animation: drift-b 42s ease-in-out infinite; }
        .blob-3 { width: 46rem; height: 46rem; bottom: -22rem; left: 28%; background: radial-gradient(circle, rgba(14, 165, 233, 0.42), transparent 66%); animation: drift-c 38s ease-in-out infinite; }
        .blob-4 { width: 34rem; height: 34rem; bottom: -12rem; right: -8rem; background: radial-gradient(circle, rgba(99, 102, 241, 0.30), transparent 66%); animation: drift-b 46s ease-in-out infinite reverse; }
        [data-theme="dark"] .blob-1 { background: radial-gradient(circle, rgba(13, 148, 136, 0.52), transparent 66%); }
        [data-theme="dark"] .blob-2 { background: radial-gradient(circle, rgba(16, 185, 129, 0.42), transparent 66%); }
        [data-theme="dark"] .blob-3 { background: radial-gradient(circle, rgba(14, 165, 233, 0.36), transparent 66%); }
        [data-theme="dark"] .blob-4 { background: radial-gradient(circle, rgba(99, 102, 241, 0.28), transparent 66%); }
        @keyframes drift-a { 0%, 100% { transform: translate3d(0, 0, 0) scale(1); } 50% { transform: translate3d(5%, 4%, 0) scale(1.1); } }
        @keyframes drift-b { 0%, 100% { transform: translate3d(0, 0, 0) scale(1.05); } 50% { transform: translate3d(-6%, 5%, 0) scale(0.95); } }
        @keyframes drift-c { 0%, 100% { transform: translate3d(0, 0, 0) scale(1); } 50% { transform: translate3d(4%, -5%, 0) scale(1.12); } }

        /* Grain halus supaya gradient tidak terlihat "flat" / banding */
        .grain {
            position: absolute;
            inset: 0;
            opacity: 0.4;
            background-image: url("data:image/svg+xml;charset=utf-8,%3Csvg xmlns='http://www.w3.org/2000/svg' width='160' height='160'%3E%3Cfilter id='n'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='0.85' numOctaves='3'/%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23n)' opacity='0.35'/%3E%3C/svg%3E");
        }
        [data-theme="dark"] .grain { opacity: 0.18; }

        /* ==========================================================
           4. GLASS SURFACE
           ========================================================== */
        .glass {
            background: var(--surface);
            /* Rim putih (kaca) + outline gelap (kontras) -> dua sisi tepi,
               persis seperti tepi Panel kaca sungguhan. */
            border: 1px solid var(--border);
            box-shadow: inset 0 0 0 1px var(--outline), inset 0 1px 0 var(--highlight), var(--shadow-card);
            /* saturate tinggi = warna mesh di belakang ikut diperkuat, ini yang
               membuat kaca terasa hidup. blur besar = tepi objek di belakang
               benar-benar melembut. */
            backdrop-filter: blur(30px) saturate(200%);
            -webkit-backdrop-filter: blur(30px) saturate(200%);
            transition: box-shadow 0.35s cubic-bezier(0.4, 0, 0.2, 1), border-color 0.35s cubic-bezier(0.4, 0, 0.2, 1), transform 0.35s cubic-bezier(0.4, 0, 0.2, 1);
        }
        .glass-hover:hover {
            transform: translateY(-3px);
            border-color: var(--outline-strong);
            box-shadow: inset 0 0 0 1px var(--outline-strong), inset 0 1px 0 var(--highlight), var(--shadow-lift);
        }
        .glass-bar {
            background: var(--surface-bar);
            border-bottom: 1px solid var(--outline);
            backdrop-filter: blur(34px) saturate(200%);
            -webkit-backdrop-filter: blur(34px) saturate(200%);
        }

        /* Kilau tipis di tepi atas kartu (efek kaca premium) */
        .edge-top::before {
            content: '';
            position: absolute;
            inset: 0 0 auto 0;
            height: 1px;
            background: linear-gradient(90deg, transparent, var(--highlight) 22%, var(--highlight) 78%, transparent);
            pointer-events: none;
        }

        /* ==========================================================
           5. TIPOGRAFI
           ========================================================== */
        .eyebrow { font-size: 10px; font-weight: 700; letter-spacing: 0.14em; text-transform: uppercase; color: var(--text-subtle); }
        .t-muted { color: var(--text-muted); }
        .t-subtle { color: var(--text-subtle); }
        .t-pos { color: var(--positive); }
        .t-neg { color: var(--negative); }
        .t-warn { color: var(--warning); }
        .t-accent { color: var(--accent); }
        .brand-text { background: linear-gradient(100deg, var(--accent) 0%, var(--accent-2) 55%, var(--accent) 100%); -webkit-background-clip: text; background-clip: text; color: transparent; }
        .rule { height: 1px; background: linear-gradient(90deg, transparent, var(--border-strong), transparent); }

        /* ==========================================================
           6. KOMPONEN: tombol, input, select, chip, slider
           ========================================================== */
        .btn {
            display: inline-flex; align-items: center; justify-content: center; gap: 0.5rem;
            border-radius: 0.875rem; font-weight: 700; font-size: 0.8125rem;
            padding: 0.7rem 1rem; transition: all 0.22s ease; cursor: pointer; user-select: none;
        }
        .btn:active { transform: scale(0.975); }
        .btn-primary {
            background: linear-gradient(135deg, var(--accent) 0%, var(--accent-2) 100%);
            color: var(--accent-contrast);
            box-shadow: 0 10px 22px -12px var(--accent);
        }
        .btn-primary:hover { filter: brightness(1.06); box-shadow: 0 14px 28px -12px var(--accent); }
        .btn-danger { background: color-mix(in srgb, var(--negative) 12%, transparent); color: var(--negative); border: 1px solid color-mix(in srgb, var(--negative) 35%, transparent); }
        .btn-danger:hover { background: color-mix(in srgb, var(--negative) 20%, transparent); }
        .btn-stop { background: linear-gradient(135deg, var(--negative) 0%, color-mix(in srgb, var(--negative) 60%, #f472b6) 100%); color: #fff; box-shadow: 0 10px 22px -12px var(--negative); }
        .btn-stop:hover { filter: brightness(1.06); }
        .btn-ghost { background: var(--field-bg); color: var(--text-muted); border: 1px solid var(--outline); }
        .btn-ghost:hover { color: var(--text); border-color: var(--border-strong); }

        .field {
            width: 100%;
            background: var(--field-bg);
            border: 1px solid var(--outline);
            border-radius: 0.875rem;
            padding: 0.7rem 1rem;
            font-size: 1rem;
            color: var(--text);
            transition: border-color 0.2s ease, box-shadow 0.2s ease, background 0.2s ease;
        }
        .field:focus { outline: none; border-color: color-mix(in srgb, var(--accent) 55%, transparent); box-shadow: var(--ring); background: var(--surface-2); }
        .field-pos { color: var(--positive); border-color: color-mix(in srgb, var(--positive) 28%, transparent); }
        .field-neg { color: var(--negative); border-color: color-mix(in srgb, var(--negative) 28%, transparent); }

        .select {
            width: 100%;
            appearance: none; -webkit-appearance: none;
            background-color: var(--field-bg);
            background-image: url("data:image/svg+xml;charset=utf-8,%3Csvg xmlns='http://www.w3.org/2000/svg' fill='none' viewBox='0 0 24 24' stroke='%2364748b' stroke-width='2.5'%3E%3Cpath stroke-linecap='round' stroke-linejoin='round' d='M19 9l-7 7-7-7'/%3E%3C/svg%3E");
            background-repeat: no-repeat; background-position: right 0.9rem center; background-size: 1rem;
            border: 1px solid var(--outline);
            border-radius: 0.875rem;
            padding: 0.65rem 2.4rem 0.65rem 1rem;
            font-size: 0.875rem; font-weight: 600;
            color: var(--text);
            cursor: pointer;
            transition: border-color 0.2s ease, box-shadow 0.2s ease;
        }
        .select:focus { outline: none; border-color: color-mix(in srgb, var(--accent) 55%, transparent); box-shadow: var(--ring); }
        .opt { background: #ffffff; color: #0f2233; }
        [data-theme="dark"] .opt { background: #101a2b; color: #e7eef6; }

        .chip { display: inline-flex; align-items: center; gap: 0.35rem; padding: 0.3rem 0.65rem; border-radius: 0.6rem; font-size: 10px; font-weight: 700; letter-spacing: 0.06em; border: 1px solid transparent; }
        .seg { display: inline-flex; align-items: center; justify-content: center; padding: 0.6rem 0.75rem; border-radius: 0.8rem; font-size: 12px; font-weight: 700; letter-spacing: 0.02em; background: var(--field-bg); color: var(--text-muted); border: 1px solid var(--outline); cursor: pointer; transition: all .18s ease; }
        .seg:hover { color: var(--text); border-color: var(--border-strong); }
        .seg-on { background: color-mix(in srgb, var(--accent) 14%, transparent); color: var(--accent); border-color: color-mix(in srgb, var(--accent) 38%, transparent); box-shadow: 0 0 0 1px color-mix(in srgb, var(--accent) 18%, transparent); }
        .chip-long { background: color-mix(in srgb, var(--accent) 14%, transparent); color: var(--accent); border-color: color-mix(in srgb, var(--accent) 32%, transparent); }
        .chip-short { background: color-mix(in srgb, var(--negative) 12%, transparent); color: var(--negative); border-color: color-mix(in srgb, var(--negative) 30%, transparent); }
        .chip-idle { background: var(--pane); color: var(--text-subtle); border-color: var(--pane-border); }
        .chip-trail { background: color-mix(in srgb, var(--warning) 14%, transparent); color: var(--warning); border-color: color-mix(in srgb, var(--warning) 34%, transparent); }
        .chip-lock { background: color-mix(in srgb, var(--negative) 12%, transparent); color: var(--negative); border-color: color-mix(in srgb, var(--negative) 32%, transparent); }

        /* Toggle switch untuk fitur On/Off (F1, F2, F4) */
        .switch { position: relative; display: inline-flex; align-items: center; flex-shrink: 0; cursor: pointer; }
        .switch .switch-track {
            position: relative; width: 44px; height: 25px; border-radius: 9999px;
            background: var(--pane-border); border: 1px solid var(--outline-strong);
            transition: background 0.25s ease, border-color 0.25s ease;
        }
        .switch .switch-track::after {
            content: ''; position: absolute; top: 2px; left: 2px; width: 19px; height: 19px;
            border-radius: 50%; background: var(--surface-2);
            box-shadow: 0 2px 5px rgba(15,23,42,0.35);
            transition: transform 0.25s ease;
        }
        .switch input:checked + .switch-track {
            background: linear-gradient(135deg, var(--accent) 0%, var(--accent-2) 100%);
            border-color: transparent;
        }
        .switch input:checked + .switch-track::after { transform: translateX(19px); }
        .sr-only { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip: rect(0,0,0,0); white-space: nowrap; border: 0; }

        .badge { display: inline-block; padding: 0.2rem 0.5rem; border-radius: 0.45rem; font-size: 10px; font-weight: 700; }
        .badge-ok { background: color-mix(in srgb, var(--positive) 14%, transparent); color: var(--positive); }
        .badge-wait { background: color-mix(in srgb, var(--warning) 16%, transparent); color: var(--warning); }

        /* Panel KPI di dalam kartu: kaca yang lebih OPAQUE dari kartu induk,
           jadi ada hierarki (kartu -> panel) dan tidak menyatu jadi satu bidang. */
        .tile {
            background: var(--pane);
            border: 1px solid var(--pane-border);
            box-shadow: inset 0 1px 2px rgba(15, 23, 42, 0.06);
            backdrop-filter: blur(14px) saturate(160%);
            -webkit-backdrop-filter: blur(14px) saturate(160%);
            border-radius: 1rem;
            padding: 0.85rem 1rem;
        }

        .slider-pro {
            -webkit-appearance: none; appearance: none;
            width: 100%; height: 6px; border-radius: 9999px;
            background: linear-gradient(90deg, var(--accent) 0%, var(--accent-2) 100%);
            outline: none; cursor: pointer;
        }
        .slider-pro::-webkit-slider-thumb {
            -webkit-appearance: none; appearance: none;
            width: 22px; height: 22px; border-radius: 50%;
            background: var(--surface-2);
            border: 3px solid var(--accent);
            box-shadow: 0 6px 14px -4px var(--accent);
            transition: transform 0.15s ease;
        }
        .slider-pro::-webkit-slider-thumb:hover { transform: scale(1.12); }
        .slider-pro::-moz-range-thumb {
            width: 20px; height: 20px; border-radius: 50%;
            background: var(--surface-2); border: 3px solid var(--accent);
            box-shadow: 0 6px 14px -4px var(--accent); cursor: pointer;
        }

        .notice { border-radius: 1rem; padding: 0.75rem 0.9rem; font-size: 11px; line-height: 1.5; display: flex; gap: 0.55rem; align-items: flex-start; }
        .notice-ok { background: color-mix(in srgb, var(--positive) 10%, transparent); border: 1px solid color-mix(in srgb, var(--positive) 28%, transparent); color: var(--positive); }
        .notice-bad { background: color-mix(in srgb, var(--negative) 10%, transparent); border: 1px solid color-mix(in srgb, var(--negative) 28%, transparent); color: var(--negative); }
        .notice-warn { background: color-mix(in srgb, var(--warning) 12%, transparent); border: 1px solid color-mix(in srgb, var(--warning) 30%, transparent); color: var(--warning); }

        /* Log box sengaja dibuat RESESIF (lebih gelap dari kartu) supaya teks log
           tetap terbaca dan area ini jelas berbeda dari panel KPI. */
        .logbox {
            background: var(--pane-sunken);
            border: 1px solid var(--outline);
            box-shadow: inset 0 2px 8px rgba(15, 23, 42, 0.08);
            border-radius: 1rem;
            padding: 1rem;
            height: 15rem;
            overflow-y: auto;
            font-size: 11px;
            line-height: 1.6;
            color: var(--accent);
        }
        .logbox::-webkit-scrollbar { width: 6px; }
        .logbox::-webkit-scrollbar-track { background: transparent; }
        .logbox::-webkit-scrollbar-thumb { background: var(--border-strong); border-radius: 9999px; }
        * { scrollbar-width: thin; scrollbar-color: var(--border-strong) transparent; }

        .toggle-track { position: relative; width: 2.75rem; height: 1.5rem; border-radius: 9999px; background: var(--field-bg); border: 1px solid var(--outline); transition: all 0.3s ease; }
        .toggle-track::after { content: ''; position: absolute; top: 50%; left: 3px; width: 1.125rem; height: 1.125rem; border-radius: 50%; background: var(--surface-2); border: 1px solid var(--border-strong); transform: translateY(-50%); transition: all 0.3s cubic-bezier(0.4, 0, 0.2, 1); box-shadow: 0 3px 8px -2px rgba(15, 23, 42, 0.35); }
        [data-theme="dark"] .toggle-track { background: color-mix(in srgb, var(--accent) 25%, transparent); border-color: color-mix(in srgb, var(--accent) 40%, transparent); }
        [data-theme="dark"] .toggle-track::after { left: calc(100% - 1.375rem); background: var(--accent); border-color: transparent; }

        .toast { position: fixed; top: 1.5rem; right: 1.5rem; z-index: 60; padding: 0.85rem 1.25rem; border-radius: 1rem; font-size: 0.8125rem; font-weight: 700; transition: all 0.3s ease; backdrop-filter: blur(20px); }
        .toast-ok { background: color-mix(in srgb, var(--positive) 14%, var(--surface-2)); border: 1px solid color-mix(in srgb, var(--positive) 35%, transparent); color: var(--positive); }
        .toast-bad { background: color-mix(in srgb, var(--negative) 12%, var(--surface-2)); border: 1px solid color-mix(in srgb, var(--negative) 35%, transparent); color: var(--negative); }

        @media (prefers-reduced-motion: reduce) {
            .blob { animation: none !important; }
            * { transition-duration: 0.01ms !important; }
        }
    </style>
</head>
<body class="min-h-screen p-4 md:p-6 xl:p-8" x-data="botApp()" x-init="init()" x-cloak>
    <div class="canvas">
        <div class="blob blob-1"></div>
        <div class="blob blob-2"></div>
        <div class="blob blob-3"></div>
        <div class="blob blob-4"></div>
        <div class="grain"></div>
    </div>

    <!-- ============================ HEADER ============================ -->
    <header class="glass glass-bar rounded-2xl md:rounded-3xl px-4 md:px-6 py-4 mb-5 md:mb-6 flex flex-col md:flex-row justify-between items-start md:items-center gap-4">
        <div class="flex items-center gap-3 md:gap-4">
            <div class="w-11 h-11 md:w-12 md:h-12 rounded-2xl flex items-center justify-center" style="background:linear-gradient(135deg,var(--accent) 0%,var(--accent-2) 100%);box-shadow:0 10px 24px -12px var(--accent)">
                <svg class="w-5 h-5 md:w-6 md:h-6" style="color:var(--accent-contrast)" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 10V3L4 14h7v7l9-11h-7z"></path></svg>
            </div>
            <div>
                <h1 class="text-lg md:text-2xl font-extrabold tracking-tight brand-text">OKX SCALPER PRO</h1>
                <p class="eyebrow mt-0.5" x-text="mode === 'demo' ? 'Demo Trading • Simulated Orders' : 'Live Trading • Real Funds'"></p>
            </div>
        </div>

        <div class="flex flex-wrap items-center gap-2 md:gap-3">
            <!-- Status pill -->
            <div class="flex items-center gap-2 pl-3 pr-4 py-2 rounded-full" style="background:var(--pane);border:1px solid var(--pane-border)">
                <span class="relative flex h-2 w-2">
                    <span class="animate-ping absolute inline-flex h-full w-full rounded-full opacity-75" :class="isRunning ? 't-accent' : 't-neg'" :style="isRunning ? 'background:var(--accent)' : 'background:var(--negative)'"></span>
                    <span class="relative inline-flex rounded-full h-2 w-2" :style="isRunning ? 'background:var(--accent)' : 'background:var(--negative)'"></span>
                </span>
                <span class="text-[11px] font-bold tracking-wider" :class="isRunning ? 't-accent' : 't-neg'" x-text="isRunning ? 'ENGINE ONLINE' : 'ENGINE OFFLINE'"></span>
            </div>

            <!-- Theme toggle -->
            <button @click="toggleTheme()" class="flex items-center gap-2 pl-2 pr-3 py-1.5 rounded-full glass glass-hover" :title="theme === 'light' ? 'Switch to dark mode' : 'Switch to light mode'" aria-label="Toggle color theme">
                <span class="toggle-track"></span>
                <span class="text-[11px] font-bold tracking-wider t-muted uppercase" x-text="theme === 'light' ? 'Light' : 'Dark'"></span>
            </button>

            <button @click="emergencyClose()" class="btn btn-danger">
                <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"></path></svg>
                KILL SWITCH
            </button>
        </div>
    </header>

    <!-- ============================ HERO / KPI ============================ -->
    <section class="glass glass-hover rounded-3xl edge-top relative overflow-hidden p-5 md:p-6 mb-5 md:mb-6">
        <div class="grid grid-cols-1 xl:grid-cols-12 gap-5 xl:gap-6 items-center relative">
            <div class="xl:col-span-4">
                <p class="eyebrow mb-2">Total PnL Since Start</p>
                <div class="flex items-baseline gap-2">
                    <span class="text-4xl md:text-5xl font-extrabold mono tracking-tight" :class="totalPnL >= 0 ? 't-pos' : 't-neg'">
                        <span x-text="totalPnL >= 0 ? '+' : ''"></span><span x-text="totalPnL.toFixed(2)"></span><span class="text-2xl md:text-3xl">%</span>
                    </span>
                </div>
                <p class="text-xs t-subtle mt-1.5" x-text="totalPnL >= 0 ? 'Bot berjalan profit' : 'Bot berjalan merugi'"></p>
            </div>

            <div class="xl:col-span-8 grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-3">
                <div class="tile">
                    <p class="eyebrow">Balance (USDT)</p>
                    <p class="mono text-sm font-bold mt-1.5" x-text="'$' + balance.toFixed(2)"></p>
                </div>
                <div class="tile">
                    <p class="eyebrow">PnL Hari Ini</p>
                    <p class="mono text-sm font-bold mt-1.5" :class="dailyPnL >= 0 ? 't-pos' : 't-neg'">
                        <span x-text="dailyPnL >= 0 ? '+' : ''"></span><span x-text="dailyPnL.toFixed(2)"></span>
                    </p>
                </div>
                <div class="tile">
                    <p class="eyebrow">Started</p>
                    <p class="mono text-sm font-bold mt-1.5" x-text="startTime ? new Date(startTime).toLocaleString([], { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' }) : '-'"></p>
                </div>
                <div class="tile">
                    <p class="eyebrow">Risk : Reward</p>
                    <p class="mono text-sm font-bold mt-1.5" :class="tpSlMode === 'atr' || riskReward() >= 1 ? 't-pos' : 't-neg'" x-text="tpSlMode === 'atr' ? '1 : 2.00' : '1 : ' + riskReward().toFixed(2)"></p>
                </div>
                <div class="tile">
                    <p class="eyebrow">Break-even WR</p>
                    <p class="mono text-sm font-bold mt-1.5" x-text="(tpSlMode === 'atr' ? 33.3 : breakEven()).toFixed(1) + '%'"></p>
                </div>
                <div class="tile">
                    <p class="eyebrow">Positions</p>
                    <p class="mono text-sm font-bold mt-1.5" x-text="openPositionCount + ' / ' + coins.filter(function(c){return c && c !== 'none'}).length"></p>
                </div>
            </div>
        </div>

        <!-- Peringatan besar saat daily loss limit tercapai (F2) -->
        <div x-show="lossLimitHit" class="mt-4 bg-red-500/10 border border-red-500/30 rounded-2xl px-4 py-3 flex items-center gap-3 relative">
            <svg class="w-5 h-5 text-red-500 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"></path></svg>
            <p class="text-sm font-bold text-red-500">DAILY LOSS LIMIT HIT — engine terkunci sampai 00:00 UTC. Semua posisi ditutup otomatis. Naikkan/nonaktifkan batas loss di konfigurasi untuk restart.</p>
        </div>
    </section>

    <div class="grid grid-cols-1 lg:grid-cols-12 gap-5 md:gap-6">
        <!-- ============================ SIDEBAR ============================ -->
        <div class="lg:col-span-4 space-y-5 md:space-y-6">
            <!-- Konfigurasi -->
            <div class="glass glass-hover rounded-3xl overflow-hidden relative">
                <button @click="configOpen = !configOpen; if(configOpen) startEditing()" class="w-full px-5 md:px-6 py-5 flex items-center justify-between text-left gap-3">
                    <div class="flex items-center gap-3 min-w-0">
                        <div class="w-9 h-9 rounded-xl flex items-center justify-center shrink-0" style="background:color-mix(in srgb, var(--accent) 12%, transparent);border:1px solid color-mix(in srgb, var(--accent) 25%, transparent)">
                            <svg class="w-4 h-4 t-accent" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 6V4m0 2a2 2 0 100 4m0-4a2 2 0 110 4m-6 8a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4m6 6v10m6-2a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4"></path></svg>
                        </div>
                        <div class="min-w-0">
                            <h2 class="text-base font-bold truncate">Trade Configuration</h2>
                            <p class="eyebrow mt-0.5" x-text="posSizingEnabled ? effPositionMargin().toFixed(2) + ' USDT (F1) • ' + leverage + 'x leverage' : margin + ' USDT • ' + leverage + 'x leverage'"></p>
                        </div>
                    </div>
                    <svg class="w-5 h-5 t-subtle shrink-0 transition-transform duration-300" :class="configOpen ? 'rotate-180' : ''" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"></path></svg>
                </button>

                <div x-show="configOpen" x-transition:enter="transition ease-out duration-300" x-transition:enter-start="opacity-0 -translate-y-3" x-transition:enter-end="opacity-100 translate-y-0" x-transition:leave="transition ease-in duration-200" x-transition:leave-start="opacity-100" x-transition:leave-end="opacity-0 -translate-y-3" class="px-5 md:px-6 pb-5 md:pb-6">
                    <div class="rule mb-5"></div>
                    <div class="space-y-5">
                        <div>
                            <label class="eyebrow block mb-2">Margin per Trade (USDT)</label>
                            <input type="number" step="0.1" x-model.number="margin" class="field mono font-bold text-lg">
                            <p class="text-[11px] t-subtle mt-1.5 leading-relaxed">Dipakai bila <b>Position Sizing (F1)</b> di bawah dalam keadaan OFF. Saat F1 aktif, margin mengikuti F1.</p>
                        </div>

                        <div class="rule"></div>

                        <!-- ======== F1: POSITION SIZING ======== -->
                        <div class="tile">
                            <div class="flex items-center justify-between gap-3">
                                <div>
                                    <p class="eyebrow mb-0.5">F1 &middot; Position Sizing</p>
                                    <p class="text-[11px] t-subtle leading-snug">Margin per trade = % saldo atau nominal tetap.</p>
                                </div>
                                <label class="switch">
                                    <input type="checkbox" x-model="posSizingEnabled" class="sr-only">
                                    <span class="switch-track"></span>
                                </label>
                            </div>
                            <template x-if="posSizingEnabled">
                                <div class="mt-3 space-y-3">
                                    <div class="grid grid-cols-2 gap-2">
                                        <button type="button" @click="posSizingMode = 'pct'" :class="posSizingMode === 'pct' ? 'seg seg-on' : 'seg'" class="seg">% Saldo</button>
                                        <button type="button" @click="posSizingMode = 'fixed'" :class="posSizingMode === 'fixed' ? 'seg seg-on' : 'seg'" class="seg">Nominal (USDT)</button>
                                    </div>
                                    <div>
                                        <label class="eyebrow block mb-2" x-text="posSizingMode === 'pct' ? 'Persentase Saldo (%)' : 'Nominal per Trade (USDT)'"></label>
                                        <input type="number" step="0.5" min="0.1" x-model.number="posSizingValue" class="field mono font-bold text-lg">
                                    </div>
                                    <template x-if="posSizingMode === 'pct'">
                                        <div>
                                            <p class="text-[11px] t-subtle mb-1">Margin per trade saat ini:</p>
                                            <p class="mono text-sm font-extrabold" :class="posSizingPctErr() ? 't-neg' : 't-pos'"
                                               x-text="'$' + effPositionMargin().toFixed(2) + '  (≤ 100% wajib)'"></p>
                                            <p x-show="posSizingPctErr()" class="text-[11px] t-neg font-bold mt-1">Input % harus ≤ 100.</p>
                                        </div>
                                    </template>
                                    <template x-if="posSizingMode === 'fixed'">
                                        <div>
                                            <p class="text-[11px] t-subtle mb-1">Margin per trade saat ini:</p>
                                            <p class="mono text-sm font-extrabold" :class="fixedSizingErr() ? 't-neg' : 't-pos'"
                                               x-text="'$' + posSizingValue.toFixed(2) + '  (saldo: $' + balance.toFixed(2) + ')'"></p>
                                            <p x-show="fixedSizingErr()" class="text-[11px] t-neg font-bold mt-1">Nominal tidak boleh melebihi saldo akun.</p>
                                        </div>
                                    </template>
                                </div>
                            </template>
                        </div>

                        <!-- ======== F2: DAILY LOSS LIMIT ======== -->
                        <div class="tile">
                            <div class="flex items-center justify-between gap-3">
                                <div>
                                    <p class="eyebrow mb-0.5">F2 &middot; Daily Loss Limit</p>
                                    <p class="text-[11px] t-subtle leading-snug">Shutdown otomatis + tutup semua posisi setiap 00:00 UTC.</p>
                                </div>
                                <label class="switch">
                                    <input type="checkbox" x-model="lossLimitEnabled" class="sr-only">
                                    <span class="switch-track"></span>
                                </label>
                            </div>
                            <div x-show="lossLimitEnabled" class="mt-3 space-y-3">
                                <div class="grid grid-cols-2 gap-2">
                                    <button type="button" @click="lossLimitMode = 'pct'" :class="lossLimitMode === 'pct' ? 'seg seg-on' : 'seg'" class="seg">% (basis F1/saldo)</button>
                                    <button type="button" @click="lossLimitMode = 'fixed'" :class="lossLimitMode === 'fixed' ? 'seg seg-on' : 'seg'" class="seg">Nominal USDT</button>
                                </div>
                                <div>
                                    <label class="eyebrow block mb-2" x-text="lossLimitMode === 'pct' ? 'Batas Loss (%)' : 'Batas Loss (USDT)'"></label>
                                    <input type="number" step="0.5" min="0.1" x-model.number="lossLimitValue" class="field field-neg mono font-bold text-lg">
                                </div>
                                <div class="notice" :class="dailyLossLimitVal() > 0 ? 'notice-ok' : 'notice-bad'">
                                    <div class="flex-1">
                                        <div class="flex items-center justify-between gap-2">
                                            <span class="font-bold uppercase tracking-wide text-[10px]">Status Harian</span>
                                        </div>
                                        <div class="mt-1 space-y-0.5">
                                            <p class="text-[11px] leading-relaxed">PnL hari ini: <b class="mono" :class="dailyPnL >= 0 ? 't-pos' : 't-neg'" x-text="(dailyPnL >= 0 ? '+' : '') + dailyPnL.toFixed(2) + ' USDT'"></b></p>
                                            <p class="text-[11px] leading-relaxed">Batas loss: <b class="mono" x-text="'-' + dailyLossLimitVal().toFixed(2) + ' USDT'"></b></p>
                                            <p x-show="lossLimitHit" class="font-bold mt-1 text-[11px]"><span class="t-neg">ENGINE TERKUNCI</span> sampai 00:00 UTC — naikkan/nonaktifkan batas untuk restart.</p>
                                        </div>
                                    </div>
                                </div>
                                <p class="text-[11px] t-subtle leading-relaxed"><b>Mode %:</b> basis = nilai Position Sizing bila F1 aktif, selain itu saldo akun. Basis menyesuaikan otomatis saat F1 diubah.</p>
                            </div>
                        </div>

                        <!-- ======== F3: TIME FILTER ======== -->
                        <div class="tile">
                            <div class="flex items-center justify-between gap-3">
                                <div>
                                    <p class="eyebrow mb-0.5">F3 &middot; Time Filter (Sesi)</p>
                                    <p class="text-[11px] t-subtle leading-snug">Entry baru hanya di dalam jendela sesi (UTC).</p>
                                </div>
                                <span class="chip" :class="timeFilterActiveNow() ? 'chip-idle' : 'chip-lock'" x-text="timeFilterActiveNow() ? 'SESI AKTIF' : 'DI LUAR SESI'"></span>
                            </div>
                            <div class="mt-3">
                                <select x-model="timeFilterMode" class="select">
                                    <option value="24/7" class="opt">24/7 — Selalu aktif</option>
                                    <option value="asian" class="opt">Asian Session (00:00–08:00 UTC)</option>
                                    <option value="london" class="opt">London Session (08:00–16:00 UTC)</option>
                                    <option value="newyork" class="opt">New York Session (13:00–21:00 UTC)</option>
                                    <option value="overlap" class="opt">NY–London Overlap (13:00–16:00 UTC)</option>
                                    <option value="custom" class="opt">Custom (input jam UTC)</option>
                                </select>
                                <template x-if="timeFilterMode === 'custom'">
                                    <div class="grid grid-cols-2 gap-3 mt-3">
                                        <div>
                                            <label class="eyebrow block mb-2">Mulai (UTC, jam 0–23)</label>
                                            <input type="number" step="1" min="0" max="23" x-model.number="customStartHour" class="field mono font-bold text-lg">
                                        </div>
                                        <div>
                                            <label class="eyebrow block mb-2">Selesai (UTC, jam 0–23)</label>
                                            <input type="number" step="1" min="0" max="23" x-model.number="customEndHour" class="field mono font-bold text-lg">
                                        </div>
                                    </div>
                                </template>
                                <p class="text-[11px] t-subtle leading-relaxed mt-2">Posisi yang sudah terbuka <b>tetap dikelola</b> (TP/SL &amp; trailing) di luar sesi. Mendukung jendela lintas tengah malam (mis. 22:00–02:00).</p>
                            </div>
                        </div>

                        <!-- ======== F4: TRAILING STOP ======== -->
                        <div class="tile">
                            <div class="flex items-center justify-between gap-3">
                                <div>
                                    <p class="eyebrow mb-0.5">F4 &middot; Trailing Stop</p>
                                    <p class="text-[11px] t-subtle leading-snug">Kunci profit: SL pindah ke entry lalu mengikuti harga.</p>
                                </div>
                                <label class="switch">
                                    <input type="checkbox" x-model="trailingEnabled" class="sr-only">
                                    <span class="switch-track"></span>
                                </label>
                            </div>
                            <template x-if="trailingEnabled">
                                <div class="mt-3 space-y-3">
                                    <div>
                                        <label class="eyebrow block mb-2">Trigger Profit (%)</label>
                                        <input type="number" step="0.1" min="0.05" x-model.number="trailingTriggerPct" class="field field-pos mono font-bold text-lg">
                                        <p class="text-[11px] t-subtle mt-1.5">Saat profit ≥ trigger, SL dipindah ke harga entry (break-even).</p>
                                    </div>
                                    <div>
                                        <label class="eyebrow block mb-2">Trailing Distance (%)</label>
                                        <input type="number" step="0.05" min="0.17" max="20" x-model.number="trailingDistPct" class="field field-neg mono font-bold text-lg">
                                        <p class="text-[11px] t-subtle mt-1.5">SL = harga ekstrem − jarak ini. Wajib &gt; 0.17% (biaya round-trip OKX ~0.16%) agar tidak tersentuh hanya karena fee. SL hanya bergerak menguntungkan, tidak pernah mundur.</p>
                                    </div>
                                </div>
                            </template>
                        </div>

                        <div class="rule"></div>

                        <div class="flex flex-col sm:flex-row gap-3 pt-1">

                        <div>
                            <div class="flex justify-between items-center mb-2.5">
                                <label class="eyebrow">Leverage</label>
                                <span class="mono text-sm font-extrabold t-accent" x-text="leverage + 'x'"></span>
                            </div>
                            <input type="range" min="1" max="50" x-model.number="leverage" class="slider-pro">
                            <div class="flex justify-between mt-1.5 text-[10px] font-bold t-subtle"><span>1x</span><span>25x</span><span>50x</span></div>
                        </div>

                        <div>
                            <label class="eyebrow block mb-2.5">Metode Penempatan SL/TP</label>
                            <div class="grid grid-cols-2 gap-2">
                                <button type="button" @click="tpSlMode = 'percent'" :class="tpSlMode === 'percent' ? 'seg seg-on' : 'seg'" class="seg">Persentase</button>
                                <button type="button" @click="tpSlMode = 'atr'" :class="tpSlMode === 'atr' ? 'seg seg-on' : 'seg'" class="seg">ATR (Volatilitas)</button>
                            </div>
                            <p class="text-[11px] t-subtle leading-relaxed mt-2">
                                <template x-if="tpSlMode === 'atr'">
                                    <span><b class="t-accent">ATR:</b> SL mengikuti volatilitas pasar terkini, TP = 2&times; jarak SL (rasio selalu 1:2).</span>
                                </template>
                                <template x-if="tpSlMode !== 'atr'">
                                    <span><b class="t-accent">Persentase:</b> SL/TP dengan jarak persen tetap.</span>
                                </template>
                            </p>
                        </div>

                        <div x-show="tpSlMode === 'percent'" class="grid grid-cols-2 gap-3">
                            <div>
                                <label class="eyebrow block mb-2">Stop Loss (%)</label>
                                <input type="number" step="0.1" min="0.1" max="50" x-model.number="stopLossPct" class="field field-neg mono font-bold text-lg">
                            </div>
                            <div>
                                <label class="eyebrow block mb-2">Take Profit (%)</label>
                                <input type="number" step="0.1" min="0.1" max="50" x-model.number="takeProfitPct" class="field field-pos mono font-bold text-lg">
                            </div>
                        </div>

                        <div x-show="tpSlMode === 'atr'" class="grid grid-cols-2 gap-3">
                            <div>
                                <label class="eyebrow block mb-2">Periode ATR</label>
                                <input type="number" step="1" min="2" max="200" x-model.number="atrPeriod" class="field mono font-bold text-lg">
                            </div>
                            <div>
                                <label class="eyebrow block mb-2">SL = ATR &times; <span x-text="atrSlMult"></span></label>
                                <input type="number" step="0.1" min="0.1" max="5" x-model.number="atrSlMult" class="field field-neg mono font-bold text-lg">
                            </div>
                        </div>
                        <p x-show="tpSlMode === 'atr'" class="text-[11px] t-subtle leading-relaxed -mt-1">
                            SL ditempatkan <b>sejauh ATR &times; <span x-text="atrSlMult"></span></b> dari harga masuk (di luar noise pasar),
                            TP di <b>2 &times; jarak SL</b>. Level dinamis: menyempit saat pasar sepi, melebar saat volatil.
                            Bila data ATR belum cukup, bot otomatis memakai persen di bawah.
                        </p>

                        <div class="notice" :class="tpSlMode === 'atr' || riskReward() >= 1 ? 'notice-ok' : 'notice-bad'">
                            <svg class="w-4 h-4 shrink-0 mt-px" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z"></path></svg>
                            <div class="flex-1">
                                <div class="flex items-center justify-between gap-2">
                                    <span class="font-bold uppercase tracking-wide text-[10px]">Risk : Reward</span>
                                    <span class="mono font-extrabold" x-text="tpSlMode === 'atr' ? '1 : 2.00' : '1 : ' + riskReward().toFixed(2)"></span>
                                </div>
                                <div class="mt-1 opacity-85">
                                    <template x-if="tpSlMode === 'atr'">
                                        <span>Rasio tetap 1:2 — TP = 2&times; SL. Win rate minimum <span class="font-bold">33.3%</span></span>
                                    </template>
                                    <template x-if="tpSlMode !== 'atr'">
                                        <span>Win rate minimum <span class="font-bold" x-text="breakEven().toFixed(1) + '%'"></span></span>
                                        <span x-show="riskReward() < 1" class="block font-bold mt-0.5">Rasio terbalik! TP harus ≥ SL</span>
                                        <span x-show="riskReward() >= 1" class="block">Hadiah <span x-text="takeProfitPct"></span>% vs risiko <span x-text="stopLossPct"></span>%</span>
                                    </template>
                                </div>
                            </div>
                        </div>

                        <div>
                            <p class="eyebrow mb-2.5">Instruments (5 Altcoins)</p>
                            <div class="space-y-2">
                                <template x-for="(coin, index) in coins" :key="index">
                                    <div class="flex items-center gap-2.5">
                                        <span class="mono text-[10px] font-bold t-subtle w-8 shrink-0" x-text="'#' + (index + 1)"></span>
                                        <select x-model="coins[index]" class="select">
                                            <option value="none" class="opt">None</option>
                                            <option value="1INCH-USDT-SWAP" class="opt">1INCH</option>
                                            <option value="AAVE-USDT-SWAP" class="opt">AAVE</option>
                                            <option value="ADA-USDT-SWAP" class="opt">ADA</option>
                                            <option value="ALGO-USDT-SWAP" class="opt">ALGO</option>
                                            <option value="ARB-USDT-SWAP" class="opt">ARB</option>
                                            <option value="ATOM-USDT-SWAP" class="opt">ATOM</option>
                                            <option value="AVAX-USDT-SWAP" class="opt">AVAX</option>
                                            <option value="AXS-USDT-SWAP" class="opt">AXS</option>
                                            <option value="BCH-USDT-SWAP" class="opt">BCH</option>
                                            <option value="BNB-USDT-SWAP" class="opt">BNB</option>
                                            <option value="BTC-USDT-SWAP" class="opt">BTC</option>
                                            <option value="CELO-USDT-SWAP" class="opt">CELO</option>
                                            <option value="CHZ-USDT-SWAP" class="opt">CHZ</option>
                                            <option value="COMP-USDT-SWAP" class="opt">COMP</option>
                                            <option value="CRV-USDT-SWAP" class="opt">CRV</option>
                                            <option value="DOGE-USDT-SWAP" class="opt">DOGE</option>
                                            <option value="DOT-USDT-SWAP" class="opt">DOT</option>
                                            <option value="ETC-USDT-SWAP" class="opt">ETC</option>
                                            <option value="ETH-USDT-SWAP" class="opt">ETH</option>
                                            <option value="FIL-USDT-SWAP" class="opt">FIL</option>
                                            <option value="ICP-USDT-SWAP" class="opt">ICP</option>
                                            <option value="IOTA-USDT-SWAP" class="opt">IOTA</option>
                                            <option value="KSM-USDT-SWAP" class="opt">KSM</option>
                                            <option value="LINK-USDT-SWAP" class="opt">LINK</option>
                                            <option value="LTC-USDT-SWAP" class="opt">LTC</option>
                                            <option value="NEAR-USDT-SWAP" class="opt">NEAR</option>
                                            <option value="PEPE-USDT-SWAP" class="opt">PEPE</option>
                                            <option value="SAND-USDT-SWAP" class="opt">SAND</option>
                                            <option value="SOL-USDT-SWAP" class="opt">SOL</option>
                                            <option value="SUSHI-USDT-SWAP" class="opt">SUSHI</option>
                                            <option value="XLM-USDT-SWAP" class="opt">XLM</option>
                                            <option value="XRP-USDT-SWAP" class="opt">XRP</option>
                                            <option value="XTZ-USDT-SWAP" class="opt">XTZ</option>
                                            <option value="YFI-USDT-SWAP" class="opt">YFI</option>
                                            <option value="ZIL-USDT-SWAP" class="opt">ZIL</option>
                                        </select>
                                    </div>
                                </template>
                            </div>
                        </div>

                        <div class="flex flex-col sm:flex-row gap-3 pt-1">
                            <button @click="saveConfig()" class="btn btn-primary flex-1">
                                <svg class="w-4 h-4 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2.5" d="M5 13l4 4L19 7"></path></svg>
                                Save Configuration
                            </button>
                            <button @click="toggleBot()" :class="isRunning ? 'btn-stop' : 'btn-primary'" class="btn flex-1">
                                <svg class="w-4 h-4 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 6v12m6-6H6"></path></svg>
                                <span x-text="isRunning ? 'STOP ENGINE' : 'START ENGINE'"></span>
                            </button>
                        </div>
                    </div>
                </div>
            </div>

            <!-- Logs -->
            <div class="glass glass-hover rounded-3xl p-5 md:p-6">
                <div class="flex items-center justify-between mb-4">
                    <h2 class="text-sm font-bold flex items-center gap-2">
                        <span class="relative flex h-2 w-2">
                            <span class="animate-ping absolute inline-flex h-full w-full rounded-full opacity-75" style="background:var(--accent)"></span>
                            <span class="relative inline-flex rounded-full h-2 w-2" style="background:var(--accent)"></span>
                        </span>
                        LIVE LOGS
                    </h2>
                    <span class="eyebrow mono" x-text="logs.length + ' entries'"></span>
                </div>
                <div class="logbox">
                    <template x-for="(log, index) in logs" :key="index">
                        <div class="mb-1.5 break-all leading-relaxed pl-2.5 border-l-2" style="border-color:color-mix(in srgb, var(--accent) 35%, transparent)" x-text="log"></div>
                    </template>
                    <div x-show="!logs.length" class="t-subtle italic">Menunggu log…</div>
                </div>
            </div>
        </div>

        <!-- ============================ COIN CARDS ============================ -->
        <div class="lg:col-span-8 grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4 md:gap-5 self-start">
            <template x-for="(coin, index) in displayCoins" :key="coin">
                <article x-show="coin !== 'none' && coin !== ''" class="glass glass-hover rounded-3xl edge-top relative overflow-hidden p-5 flex flex-col"
                         :style="!coinStates[coin]?.isValidSize ? 'border-color:color-mix(in srgb, var(--negative) 45%, transparent)' : ''">
                    <!-- cahaya lembut sesuai arah posisi -->
                    <div class="absolute -top-16 -right-16 w-40 h-40 rounded-full blur-3xl pointer-events-none"
                         :style="coinStates[coin]?.position === 'LONG' ? 'background:color-mix(in srgb, var(--accent) 22%, transparent)'
                               : (coinStates[coin]?.position === 'SHORT' ? 'background:color-mix(in srgb, var(--negative) 18%, transparent)' : 'background:var(--pane)')"></div>

                    <header class="flex justify-between items-start gap-2 mb-5 relative">
                        <div class="flex items-center gap-3 min-w-0">
                            <div class="w-10 h-10 rounded-xl flex items-center justify-center shrink-0 font-extrabold text-sm"
                                 :style="'background:var(--pane);border:1px solid var(--pane-border)'"
                                 x-text="coin.split('-')[0].substring(0, 2)"></div>
                            <div class="min-w-0">
                                <h3 class="text-base font-extrabold tracking-tight truncate" x-text="coin.split('-')[0]"></h3>
                                <p class="eyebrow mt-0.5">PERPETUAL</p>
                            </div>
                        </div>
                        <span class="chip shrink-0"
                              :class="coinStates[coin]?.position === 'LONG' ? 'chip-long' : (coinStates[coin]?.position === 'SHORT' ? 'chip-short' : 'chip-idle')"
                              x-text="coinStates[coin]?.position || 'IDLE'"></span>
                        <!-- Trailing stop aktif (F4) -->
                        <span x-show="coinStates[coin]?.trailActive" class="chip chip-trail shrink-0">
                            <svg class="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2.5" d="M13 7h8m0 0v8m0-8l-8 8-4-4-6 6"></path></svg>
                            TRAIL
                        </span>
                    </header>

                    <div class="mb-5 relative">
                        <p class="text-2xl md:text-[1.7rem] font-extrabold mono tracking-tight" x-text="'$' + (coinStates[coin]?.price?.toFixed(6) || '0.000000')"></p>
                        <p class="text-[11px] font-bold mt-1 t-accent mono min-h-[1rem]" x-text="coinStates[coin]?.signal || ''"></p>
                        <p class="mt-2 flex items-baseline gap-1.5">
                            <span class="text-base font-extrabold mono" :class="(coinStates[coin]?.pnl || 0) >= 0 ? 't-pos' : 't-neg'">
                                <span x-text="(coinStates[coin]?.pnl || 0) >= 0 ? '+' : ''"></span><span x-text="(coinStates[coin]?.pnl?.toFixed(2) || '0.00')"></span>%
                            </span>
                            <span class="text-[11px] font-semibold t-subtle mono" x-text="'(' + Math.round((coinStates[coin]?.pnl || 0) * (coinStates[coin]?.notional || 0) / 100 * 100) / 100 + ' USDT)'"></span>
                        </p>
                    </div>

                    <dl class="mt-auto text-[11px] t-muted space-y-2 pt-4 relative" style="border-top:1px solid var(--border)">
                        <div class="flex justify-between gap-2"><dt>Order Size</dt><dd class="mono font-bold t-accent" x-text="(coinStates[coin]?.orderSize || 0).toFixed(2) + ' contracts'"></dd></div>
                        <div class="flex justify-between gap-2"><dt>Margin Used</dt><dd class="mono font-bold t-accent" x-text="'$' + (coinStates[coin]?.actualMargin || 0).toFixed(2)"></dd></div>
                        <div class="flex justify-between gap-2"><dt>Notional</dt><dd class="mono font-bold" x-text="'$' + (coinStates[coin]?.notional || 0).toFixed(2)"></dd></div>
                        <div class="flex justify-between gap-2"><dt>Leverage</dt><dd class="mono font-bold" x-text="(coinStates[coin]?.leverage || leverage) + 'x'"></dd></div>
                        <div class="flex justify-between gap-2"><dt>Position</dt><dd class="mono font-bold" x-text="(coinStates[coin]?.contracts || 0).toFixed(2) + ' contracts'"></dd></div>
                        <div class="flex justify-between gap-2"><dt>Entry Price</dt><dd class="mono font-bold" x-text="coinStates[coin]?.entryPrice?.toFixed(6) || '-'"></dd></div>
                        <div class="flex justify-between gap-2"><dt>Risk : Reward</dt><dd class="mono font-bold" :class="coinRR(coin) >= 1 ? 't-pos' : 't-neg'" x-text="'1 : ' + coinRR(coin).toFixed(2)"></dd></div>

                        <template x-if="coinStates[coin]?.position && coinStates[coin]?.position !== 'NONE'">
                            <div class="space-y-2 pt-2">
                                <div class="flex justify-between gap-2"><dt>Take Profit</dt><dd class="mono font-bold t-pos" x-text="(coinStates[coin]?.tpPrice?.toFixed(6) || '-') + '  (+' + coinTpPct(coin) + '%)'"></dd></div>
                                <div class="flex justify-between gap-2"><dt>Stop Loss</dt><dd class="mono font-bold t-neg" x-text="(coinStates[coin]?.slPrice?.toFixed(6) || '-') + '  (-' + coinSlPct(coin) + '%)'"></dd></div>
                                <div class="flex justify-between items-center gap-2">
                                    <dt>Metode</dt>
                                    <dd class="mono font-bold t-subtle" x-text="(coinStates[coin]?.tpSlMode || tpSlMode).toUpperCase()"></dd>
                                </div>
                                <div class="flex justify-between items-center gap-2">
                                    <dt>TP/SL di OKX</dt>
                                    <dd class="badge" :class="coinStates[coin]?.hasTpSl ? 'badge-ok' : 'badge-wait'" x-text="coinStates[coin]?.hasTpSl ? '100% POSISI' : 'MENUNGGU'"></dd>
                                </div>
                                <div x-show="coinStates[coin]?.trailActive" class="flex justify-between items-center gap-2">
                                    <dt>Trailing</dt>
                                    <dd class="mono font-bold t-warn" x-text="'AKTIF • ekstrem ' + (coinStates[coin]?.trailExtreme?.toFixed(6) || '-')"></dd>
                                </div>
                            </div>
                        </template>
                    </dl>

                    <template x-if="!coinStates[coin]?.isValidSize">
                        <div class="notice notice-bad mt-3 relative">
                            <svg class="w-4 h-4 shrink-0 mt-px" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"></path></svg>
                            <div>
                                <span x-show="!coinStates[coin]?.wsError">Margin $<span x-text="posSizingEnabled ? effPositionMargin().toFixed(2) : margin"></span> @ <span x-text="leverage"></span>x tidak cukup. Minimal $<span x-text="coinStates[coin]?.minMarginReq?.toFixed(2)"></span> (1 lot)</span>
                                <span x-show="coinStates[coin]?.wsError" class="t-warn" x-text="coinStates[coin]?.wsError"></span>
                            </div>
                        </div>
                    </template>
                </article>
            </template>

            <div x-show="!displayCoins.length" class="glass rounded-3xl p-10 text-center md:col-span-2 xl:col-span-3">
                <p class="t-muted font-semibold">Belum ada instrumen dipantau</p>
                <p class="text-xs t-subtle mt-1">Buka Trade Configuration untuk memilih koin.</p>
            </div>
        </div>
    </div>

    <script>
        function botApp() {
            return {
                margin: 1.0, leverage: 10, mode: 'demo',
                coins: ['BTC-USDT-SWAP', 'ETH-USDT-SWAP', 'SOL-USDT-SWAP', 'DOGE-USDT-SWAP', 'PEPE-USDT-SWAP'],
                // Metode penempatan SL/TP: 'percent' (jarak persen tetap) atau 'atr'
                // (SL = ATR x atrSlMult, TP = 2 x SL -> rasio selalu 1:2)
                tpSlMode: 'atr', atrPeriod: 14, atrSlMult: 1.0,
                // Rasio risk:reward 1:2 yang BENAR -> risiko (SL) lebih kecil dari hadiah (TP)
                stopLossPct: 0.4, takeProfitPct: 0.8,
                isRunning: false, coinStates: {}, logs: [],
                totalPnL: 0, startTime: null, configOpen: false, isEditing: false,
                theme: 'light', openPositionCount: 0,
                // F1: Position Sizing (margin per trade dari % saldo / nominal tetap)
                posSizingEnabled: false, posSizingMode: 'pct', posSizingValue: 5,
                // F2: Daily Loss Limit (shutdown otomatis, reset 00:00 UTC)
                lossLimitEnabled: false, lossLimitMode: 'pct', lossLimitValue: 5,
                // F3: Time Filter (sesi UTC, hanya untuk entry baru)
                timeFilterMode: '24/7', customStartHour: 8, customEndHour: 16,
                // F4: Trailing Stop (trigger profit -> BEP, SL mengikuti harga ekstrem)
                trailingEnabled: false, trailingTriggerPct: 0.5, trailingDistPct: 0.3,
                // Data akun dari OKX (dipakai F1/F2 & tampilan)
                balance: 0, dailyPnL: 0, lossLimitHit: false, lossDay: '',
                // Rasio risiko:hadiah. < 1 berarti konfigurasi terbalik.
                riskReward() {
                    const sl = parseFloat(this.stopLossPct), tp = parseFloat(this.takeProfitPct);
                    if (!sl || sl <= 0) return 0;
                    return tp / sl;
                },
                // Win rate minimum agar expectancy tidak negatif.
                breakEven() {
                    const sl = parseFloat(this.stopLossPct), tp = parseFloat(this.takeProfitPct);
                    if (!sl || !tp || (sl + tp) <= 0) return 0;
                    return sl / (sl + tp) * 100;
                },
                // R:R per posisi — memakai jarak aktual tiap koin (berbeda tiap posisi
                // di mode ATR karena volatilitas berubah), fallback ke konfigurasi.
                coinRR(coin) {
                    const st = this.coinStates[coin];
                    if (st && st.slDistPct > 0 && st.tpDistPct > 0) return st.tpDistPct / st.slDistPct;
                    return this.riskReward();
                },
                coinTpPct(coin) {
                    const st = this.coinStates[coin];
                    return (st && st.tpDistPct > 0) ? st.tpDistPct.toFixed(2) : this.takeProfitPct;
                },
                coinSlPct(coin) {
                    const st = this.coinStates[coin];
                    return (st && st.slDistPct > 0) ? st.slDistPct.toFixed(2) : this.stopLossPct;
                },
                // ===== F1: Position Sizing =====
                // Margin per trade efektif (default / % saldo / nominal tetap).
                effPositionMargin() {
                    if (!this.posSizingEnabled) return parseFloat(this.margin) || 0;
                    if (this.posSizingMode === 'fixed') return parseFloat(this.posSizingValue) || 0;
                    return (parseFloat(this.balance) || 0) * (parseFloat(this.posSizingValue) || 0) / 100;
                },
                posSizingPctErr() {
                    const v = parseFloat(this.posSizingValue);
                    return this.posSizingEnabled && this.posSizingMode === 'pct' && (!v || v > 100);
                },
                fixedSizingErr() {
                    const v = parseFloat(this.posSizingValue);
                    const bal = parseFloat(this.balance) || 0;
                    return this.posSizingEnabled && this.posSizingMode === 'fixed' && (!v || v <= 0 || (bal > 0 && v > bal));
                },
                // ===== F2: Daily Loss Limit =====
                // Batas loss harian; basis mode % = nilai F1 bila aktif, selain itu saldo.
                dailyLossLimitVal() {
                    if (!this.lossLimitEnabled) return 0;
                    const v = parseFloat(this.lossLimitValue) || 0;
                    if (this.lossLimitMode === 'fixed') return v;
                    let base = parseFloat(this.margin) || 0;
                    if (this.posSizingEnabled) base = this.effPositionMargin();
                    else if ((parseFloat(this.balance) || 0) > 0) base = parseFloat(this.balance);
                    return base * v / 100;
                },
                // ===== F3: Time Filter =====
                // true bila waktu UTC sekarang berada di dalam jendela sesi.
                timeFilterActiveNow() {
                    const mode = this.timeFilterMode || '24/7';
                    if (mode === '24/7') return true;
                    let start, end;
                    if (mode === 'asian') { start = 0; end = 8; }
                    else if (mode === 'london') { start = 8; end = 16; }
                    else if (mode === 'newyork') { start = 13; end = 21; }
                    else if (mode === 'overlap') { start = 13; end = 16; }
                    else if (mode === 'custom') { start = parseInt(this.customStartHour) || 0; end = parseInt(this.customEndHour) || 0; }
                    else return true;
                    if (start === 0 && end === 24) return true;
                    const now = new Date();
                    const h = now.getUTCHours() + now.getUTCMinutes() / 60;
                    if (start < end) return h >= start && h < end;
                    return h >= start || h < end; // jendela lintas tengah malam
                },
                // Gabungkan koin dari konfigurasi dengan koin yang punya posisi
                // terbuka di OKX tapi tidak ada di watchlist, agar tidak pernah
                // ada posisi terbuka yang tidak terlihat di dashboard.
                get displayCoins() {
                    const list = (this.coins || []).filter(c => c && c !== 'none');
                    const orphans = Object.keys(this.coinStates || {}).filter(s => {
                        if (s === 'none' || list.includes(s)) return false;
                        const st = this.coinStates[s];
                        return st && st.position && st.position !== 'NONE';
                    });
                    return list.concat(orphans);
                },
                // Light = tema utama, dark jadi alternatif. Pilihan disimpan agar
                // konsisten di reload berikutnya.
                toggleTheme() {
                    this.theme = this.theme === 'light' ? 'dark' : 'light';
                    document.documentElement.setAttribute('data-theme', this.theme);
                    try { localStorage.setItem('okx-ui-theme', this.theme); } catch (e) {}
                },
                init() {
                    this.theme = document.documentElement.getAttribute('data-theme') || 'light';
                    setInterval(async () => {
                        try {
                            const res = await fetch('/api/state');
                            if (!res.ok) throw new Error('Failed to fetch state');
                            const data = await res.json();
                            // Only update config values if not currently editing
                            if (!this.isEditing) {
                                this.margin = data.Config.margin;
                                this.leverage = data.Config.leverage;
                                this.coins = data.Config.coins;
                                this.takeProfitPct = data.Config.takeProfitPct;
                                this.stopLossPct = data.Config.stopLossPct;
                                this.tpSlMode = data.Config.tpSlMode || 'atr';
                                this.atrPeriod = data.Config.atrPeriod || 14;
                                this.atrSlMult = data.Config.atrSlMult || 1.0;
                                this.posSizingEnabled = !!data.Config.posSizingEnabled;
                                this.posSizingMode = data.Config.posSizingMode || 'pct';
                                this.posSizingValue = data.Config.posSizingValue;
                                this.lossLimitEnabled = !!data.Config.lossLimitEnabled;
                                this.lossLimitMode = data.Config.lossLimitMode || 'pct';
                                this.lossLimitValue = data.Config.lossLimitValue;
                                this.timeFilterMode = data.Config.timeFilterMode || '24/7';
                                this.customStartHour = data.Config.customStartHour;
                                this.customEndHour = data.Config.customEndHour;
                                this.trailingEnabled = !!data.Config.trailingEnabled;
                                this.trailingTriggerPct = data.Config.trailingTriggerPct;
                                this.trailingDistPct = data.Config.trailingDistPct;
                            }
                            this.mode = data.Config.mode;
                            this.isRunning = data.Config.isRunning;
                            this.coinStates = data.Coins || {};
                            this.logs = data.Logs || [];
                            this.totalPnL = data.TotalPnL || 0;
                            this.balance = data.balance || 0;
                            this.dailyPnL = data.dailyPnL || 0;
                            this.lossLimitHit = !!data.lossLimitHit;
                            this.lossDay = data.lossDay || '';
                            this.startTime = data.StartTime || null;
                            this.openPositionCount = this.displayCoins.filter(c => {
                                const st = this.coinStates[c];
                                return st && st.position && st.position !== 'NONE';
                            }).length;
                        } catch (e) { console.error('State fetch error:', e); }
                    }, 500);
                },
                startEditing() {
                    this.isEditing = true;
                },                stopEditing() {
                    this.isEditing = false;
                },
                async saveConfig() {
                    this.isEditing = true;
                    // Validasi sisi klien agar kesalahan tidak terkirim ke server
                    if (this.tpSlMode === 'percent') {
                        if (parseFloat(this.stopLossPct) <= 0 || parseFloat(this.takeProfitPct) <= 0) {
                            this.isEditing = false;
                            this.showNotification('Stop Loss dan Take Profit harus lebih dari 0', 'error');
                            return;
                        }
                        if (this.riskReward() < 1) {
                            this.isEditing = false;
                            this.showNotification('Take Profit harus lebih besar atau sama dengan Stop Loss (risiko:hadiah minimal 1:1)', 'error');
                            return;
                        }
                    } else {
                        const p = parseInt(this.atrPeriod), m = parseFloat(this.atrSlMult);
                        if (!p || p < 2 || p > 200) {
                            this.isEditing = false;
                            this.showNotification('Periode ATR harus antara 2 dan 200', 'error');
                            return;
                        }
                        if (!m || m <= 0 || m > 5) {
                            this.isEditing = false;
                            this.showNotification('Pengali ATR harus antara 0 dan 5', 'error');
                            return;
                        }
                    }
                    // Validasi F1: Position Sizing
                    if (this.posSizingEnabled) {
                        if (this.posSizingMode === 'pct' && this.posSizingPctErr()) {
                            this.isEditing = false;
                            this.showNotification('Position Sizing % harus antara 0 dan 100', 'error');
                            return;
                        }
                        if (this.posSizingMode === 'fixed' && this.fixedSizingErr()) {
                            this.isEditing = false;
                            this.showNotification('Nominal Position Sizing tidak boleh melebihi saldo akun', 'error');
                            return;
                        }
                    }
                    // Validasi F2: Daily Loss Limit
                    if (this.lossLimitEnabled && (!parseFloat(this.lossLimitValue) || parseFloat(this.lossLimitValue) <= 0)) {
                        this.isEditing = false;
                        this.showNotification('Batas loss harian harus lebih dari 0', 'error');
                        return;
                    }
                    // Validasi F3: Custom session (UTC)
                    if (this.timeFilterMode === 'custom') {
                        const s = parseInt(this.customStartHour), e = parseInt(this.customEndHour);
                        if (isNaN(s) || isNaN(e) || s < 0 || s > 23 || e < 0 || e > 23) {
                            this.isEditing = false;
                            this.showNotification('Jam sesi custom harus antara 0 dan 23 (UTC)', 'error');
                            return;
                        }
                    }
                    // Validasi F4: Trailing Stop
                    if (this.trailingEnabled) {
                        if (!parseFloat(this.trailingTriggerPct) || parseFloat(this.trailingTriggerPct) <= 0) {
                            this.isEditing = false;
                            this.showNotification('Trigger profit trailing harus lebih dari 0', 'error');
                            return;
                        }
                        if (parseFloat(this.trailingDistPct) < 0.17) {
                            this.isEditing = false;
                            this.showNotification('Trailing distance harus lebih dari 0.17% (biaya round-trip OKX ~0.16%)', 'error');
                            return;
                        }
                    }
                    try {
                        const res = await fetch('/api/config', {
                            method: 'POST', headers: {'Content-Type': 'application/json'},
                            body: JSON.stringify({
                                margin: this.margin,
                                leverage: this.leverage,
                                coins: this.coins,
                                isRunning: this.isRunning,
                                timeframe: '5m',
                                tpSlMode: this.tpSlMode,
                                atrPeriod: parseInt(this.atrPeriod) || 14,
                                atrSlMult: parseFloat(this.atrSlMult) || 1.0,
                                takeProfitPct: parseFloat(this.takeProfitPct),
                                stopLossPct: parseFloat(this.stopLossPct),
                                posSizingEnabled: !!this.posSizingEnabled,
                                posSizingMode: this.posSizingMode,
                                posSizingValue: parseFloat(this.posSizingValue) || 0,
                                lossLimitEnabled: !!this.lossLimitEnabled,
                                lossLimitMode: this.lossLimitMode,
                                lossLimitValue: parseFloat(this.lossLimitValue) || 0,
                                timeFilterMode: this.timeFilterMode,
                                customStartHour: parseInt(this.customStartHour) || 0,
                                customEndHour: parseInt(this.customEndHour) || 0,
                                trailingEnabled: !!this.trailingEnabled,
                                trailingTriggerPct: parseFloat(this.trailingTriggerPct) || 0,
                                trailingDistPct: parseFloat(this.trailingDistPct) || 0
                            })
                        });
                        const data = await res.json().catch(() => ({}));
                        if (!res.ok) {
                            this.isEditing = false;
                            this.showNotification(data.error || 'Failed to save configuration', 'error');
                            return;
                        }
                        this.configOpen = false;
                        this.isEditing = false;
                        if (data.success) {
                            this.showNotification('Configuration saved successfully', 'success');
                        }
                    } catch (e) {
                        console.error('Config save error:', e);
                        this.isEditing = false;
                        this.showNotification('Failed to save configuration', 'error');
                    }
                },
                async toggleBot() {
                    this.isRunning = !this.isRunning;
                    await this.saveConfig();
                },
                async emergencyClose() {
                    if(confirm('KILL SWITCH: Close ALL positions and stop bot?')) {
                        try {
                            const res = await fetch('/api/emergency', { method: 'POST' });
                            if (!res.ok) throw new Error('Emergency close failed');
                            this.isRunning = false;
                        } catch (e) { console.error('Emergency close error:', e); }
                    }
                },
                showNotification(message, type) {
                    const notif = document.createElement('div');
                    notif.className = 'toast ' + (type === 'success' ? 'toast-ok' : 'toast-bad');
                    notif.textContent = message;
                    document.body.appendChild(notif);
                    setTimeout(() => {
                        notif.style.opacity = '0';
                        notif.style.transform = 'translateY(-10px)';
                        setTimeout(() => notif.remove(), 300);
                    }, 3000);
                }
            }
        }
    </script>
</body>
</html>
`
