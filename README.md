# OKX Scalper Bot

Bot dengan dashboard web responsif untuk memantau candle swap USDT OKX dan menjalankan strategi **Long/Short** yang dapat dipilih. Default strategi adalah MTF Confluence (1H/15m/5m); strategi legacy Bollinger Bands (20,2) + RSI (14) + EMA 200 juga tersedia. Default mode akun adalah **demo trading**. Mode live tersedia, tetapi mengirim order sungguhan dan berisiko kehilangan dana.

> **Peringatan:** Jangan beralih ke live sebelum menguji demo dan memverifikasi ukuran kontrak, parameter order, leverage, serta status posisi di akun OKX. Ini bukan sistem trading siap produksi.

## Daftar Isi

- [Fitur Utama](#fitur-utama)
- [Perhitungan Margin & Ukuran Order](#perhitungan-margin--ukuran-order)
- [TP/SL Entire Position](#tpsl-entire-position)
- [Fitur Manajemen Risiko (F1–F4)](#fitur-manajemen-risiko-f1f4)
- [Strategi Entry (Long/Short)](#strategi-entry-longshort)
- [Persyaratan](#persyaratan)
- [Instalasi & Deploy](#instalasi--deploy)
- [Konfigurasi API](#konfigurasi-api)
- [Mode Demo](#mode-demo)
- [Beralih ke Live](#beralih-ke-live)
- [Dashboard](#dashboard)
- [Akses dari HP (ngrok)](#akses-dari-hp-ngrok)
- [API](#api)
- [Validasi](#validasi)
- [Keamanan Credential](#keamanan-credential)

## Fitur Utama

| Fitur | Keterangan |
| --- | --- |
| **5 altcoin** | Pilih koin ke-1..5 dari dropdown. Opsi `None` untuk memakai kurang dari 5. |
| **35 koin terverifikasi** | Hanya koin yang benar-benar live di OKX **dan** tersedia di environment demo. |
| **Maks 5 posisi bersamaan** | Dihitung dari posisi nyata di OKX, bukan dari state lokal. |
| **Margin & leverage akurat** | Ukuran order dihitung dari `ctVal`/`lotSz`/`minSz` resmi OKX, bukan peta hardcode. |
| **TP/SL entire position** | TP dan SL masing-masing menutup **100%** posisi, menggantikan order di bursa. |
| **TP/SL dua metode** | `ATR (Volatilitas)` (default) atau `Persentase` — dipilih dari UI. Mode persen memakai `Take Profit %`/`Stop Loss %`; mode ATR memakai SL = `ATR × pengali`, TP = 2× jarak SL (rasio selalu 1:2). |
| **ATR dinamis** | Level SL/TP menyesuaikan volatilitas pasar 5m terkini: menyempit saat pasar sepi, melebar saat volatil. Dibatasi minimum 0.25% dan maksimum anti-likuidasi (60% dari jarak likuidasi isolated ≈ 100%/leverage). |
| **Fallback persen** | Saat riwayat ATR belum cukup (bot baru start sebelum seed REST selesai), otomatis memakai persen konfigurasi — bot tidak pernah berhenti memasang TP/SL. |
| **Watchdog TP/SL** | Setiap 10 detik bot memastikan *setiap* posisi di OKX punya TP+SL 100% **sesuai konfigurasi** — harga/ukuran yang menyimpang otomatis diganti (bukan hanya dipasang jika belum ada). Di mode ATR harga di-anchor ke pemasangan terakhir agar tidak reprice ulang tiap candle. |
| **F1 Position Sizing** | Margin per trade bisa dihitung dari **% saldo** atau **nominal tetap (USDT)** dengan toggle On/Off. Saat OFF, memakai margin tetap dari konfigurasi. |
| **F2 Daily Loss Limit** | Batas kerugian harian (realized + unrealized, sejak 00:00 UTC) memicu **shutdown otomatis + tutup semua posisi**. Reset tiap 00:00 UTC. |
| **F3 Time Filter** | Entry baru hanya di dalam jendela sesi pasar (UTC): Asian, London, New York, NY–London Overlap, atau custom. Posisi terbuka tetap dikelola di luar sesi. |
| **F4 Trailing Stop** | Native OKX (`move_order_stop`): satu order dengan `callbackRatio` (jarak) + `activePx` (harga aktivasi), digerakkan penuh oleh exchange — SL hanya bergerak menguntungkan, tidak pernah mundur. |
| **F5 Pilihan strategi** | Pilih strategi BB/RSI/EMA200 yang sudah ada, EMA+RSI, MTF Confluence (1H/15m/5m), atau Advanced SMC+Volume; tersedia gaya conservative/aggressive. |
| **Sinkronisasi OKX** | Posisi, ukuran kontrak, dan PnL diambil dari OKX tiap 10 detik. |
| **PnL riil** | Dihitung dari riwayat fill OKX (`/api/v5/trade/fills`) termasuk fee. |
| **Peringkat konfigurasi** | Instrumen yang delisted dari OKX ditolak backend dengan HTTP 400. |
| **Race-free** | Lolos `go build -race` tanpa `DATA RACE` pada operasi live. |

## Perhitungan Margin & Ukuran Order

Bot mengambil spesifikasi kontrak langsung dari OKX (`GET /api/v5/public/instruments?instType=SWAP`) saat startup dan setiap 6 jam. Dipakai:

- `ctVal` — jumlah base coin per 1 kontrak
- `lotSz` — kelipatan size order yang sah
- `minSz` — size order minimum
- `tickSz` — tick size harga (untuk pembulatan TP/SL)

Rumus:

```
notional   = margin × leverage
size       = floor(notional / (harga × ctVal) / lotSz) × lotSz
marginReal = size × harga × ctVal / leverage
```

`size` wajib kelipatan `lotSz` dan ≥ `minSz`, jika tidak order ditolak oleh OKX.

### Kenapa margin dulu tidak sesuai

Versi lama memakai peta `contractMultipliers` yang **salah total** untuk sebagian besar koin:
| SOL-USDT-SWAP | 0.1 | **1** | 10× |
| DOGE-USDT-SWAP | 10 | **1000** | 100× |
| XRP-USDT-SWAP | 1 | **100** | 100× |
| ADA-USDT-SWAP | 1 | **100** | 100× |
| PEPE-USDT-SWAP | 1 000 000 | **10 000 000** | 10× |

Akibatnya order 10–100× lebih besar dari yang diinput user, sehingga margin terpakai di OKX jauh melebihi input.

### Deviasi yang tidak bisa dihindari

Karena `size` harus kelipatan `lotSz`, margin terpakai bisa sedikit di bawah input. Deviasi rata-rata **< 1%**, batas terburuk ±8% (koin dengan `lotSz` besar dan harga sangat kecil). Angka **`Margin Used`** di dashboard adalah nilai yang benar-benar dikirim ke OKX, bukan hasil interpolasi.

Contoh terverifikasi di OKX demo:

```
Input user          : margin $20 @ 20x  →  notional $400
Order dikirim       : ADA 16.40 kontrak
Posisi di OKX       : 16.40 kontrak, avgPx 0.2438
Perhitungan         : 16.40 × (0.2438 × 100) = $399.83
Margin terpakai     : $399.83 / 20 = $19.99   ← selisih 0.05% dari $20
```

## TP/SL Entire Position

TP dan SL dikirim sebagai **dua order terpisah di OKX**, masing-masing memakai ukuran posisi penuh (entire position).

### Dua metode penempatan level

Pilih metode dari panel **Trade Configuration → Metode Penempatan SL/TP**:

| Metode | SL | TP | Kapan dipakai |
| --- | --- | --- | --- |
| **ATR (Volatilitas)** *(default)* | `clamp(ATR 5m × pengali)` di luar entry | `2 × jarak SL` → rasio **selalu 1:2** | Pasar berubah-ubah: level mengikuti volatilitas terkini. |
| **Persentase** | `Stop Loss %` tetap | `Take Profit %` tetap | Ingin level yang persis sama di semua kondisi. |

Parameter ATR (default: periode **14**, pengali **1.0**):

- `SL = ATR × pengali` — mis. ATR 0.5% × 1.0 → SL 0.5% dari entry, TP 1.0%.
- Rentang SL dijepit ke **0.25% – 5%**, dan lebih jauh ke **60% dari jarak likuidasi isolated** (≈ 100%/leverage). Batas bawah mencegah SL tersentuh noise/spread di pasar sepi; batas atas mencegah SL kalah cepat oleh likuidasi di pasar ekstrem.
- Di mode ATR, `Take Profit %`/`Stop Loss %` di UI tetap disimpan sebagai **fallback** bila data ATR belum cukup.
- ATR dihitung dari candle OHLC 5m (100 candle terakhir, di-seed dari REST saat startup sehingga langsung tersedia).

### Rasio Risk:Reward 1:2 yang benar

Mode **persen**: default `Take Profit = 0.8%` dan `Stop Loss = 0.4%` — **risiko (SL) lebih kecil dari hadiah (TP)**, sehingga untuk menang Anda hanya butuh win rate di atas **33.3%**. Mode **ATR**: TP = 2× jarak SL, jadi rasio 1:2 terjamin tanpa perlu menyetel dua angka.

> **Kesalahan versi lama:** sebelumnya TP `0.4%` dan SL `0.8%` (terbalik). Bot butuh win rate **64.8%** hanya untuk impas — dengan 47 trade aktual (win rate 68.1%) selisih keamanannya hanya 3 poin. Versi ini memperbaikinya dan menjadikannya parameter yang bisa diubah dari UI.

Invarian yang dijamin (diuji terhadap spesifikasi & harga asli OKX):

```

```json
"attachAlgoOrds": [
  { "tpTriggerPx": "<harga TP>", "tpOrdPx": "-1", "tpTriggerPxType": "last" },
  { "slTriggerPx": "<harga SL>", "slOrdPx": "-1", "slTriggerPxType": "last" }
]
```

Detail penting:

- `tpOrdPx` / `slOrdPx` = `"-1"` → order dieksekusi di harga **market** saat trigger tersentuh. Ini juga menghindari error presisi `tpTriggerPx and tpOrdPx don't match` (sCode 50016).
- Harga TP/SL dibulatkan ke `tickSize` OKX dengan arah yang menjamin invarian risiko:hadiah (lihat di atas). Arah pembulatan **sama** untuk TP dan SL: ke atas untuk LONG, ke bawah untuk SHORT.
- Semua order TP/SL memakai `reduceOnly: true` sehingga tidak pernah membuka posisi terbalik.
- **Exit 100%** memakai `POST /api/v5/trade/close-position` (bukan `order` dengan `sz`), jadi mustahil terjadi partial close karena salah hitung.
- TP/SL **tidak lagi dipantau manual oleh bot** — ini yang sebelumnya menyebabkan partial close dan exit ganda. Bot hanya mendeteksi exit lewat sinkronisasi posisi.

### Saat konfigurasi TP/SL diubah dari UI

Simpan konfigurasi dengan mode/level TP/SL baru → bot membatalkan order TP/SL lama lalu memasang yang baru untuk **semua posisi terbuka** (tidak hanya order berikutnya). Di log muncul `[TP/SL UPDATED]`. Di mode ATR, reprice memakai ATR pasar **terkini** sekali (sengaja, bukan dari watchdog).

### Watchdog

Tiap 10 detik `syncPositionsWithOKX()` memanggil `ensureTPSL()` untuk setiap posisi. Fungsi ini membandingkan TP/SL yang live di OKX dengan harga yang seharusnya:

- **Belum ada / hilang** → dibuat ulang.
- **Harga trigger atau ukuran berbeda** dari konfigurasi (mis. posisi lama dibuka dengan rasio lama) → order lama **dibatalkan lebih dulu**, lalu dipasang yang baru.
- **Cooldown 90 detik** per instrumen mencegah churn bila OKX menolak penggantian.
- Perbaikan dilakukan lewat satu jalur kode (`replaceTpSl`), jadi tidak ada risiko TP/SL ganda menumpuk.
- **Anti-churn mode ATR:** harga yang diharapkan di-anchor ke harga yang **tercatat** saat pemasangan terakhir (`CoinState.TpPrice/SlPrice`), bukan dihitung ulang dari ATR tiap candle — karena ATR berubah setiap candle dan itu akan memicu pemasangan ulang terus-menerus. Untuk posisi tanpa catatan (mis. dari sebelum restart / dibuka manual), level dihitung dari ATR sekali lalu dicatat.

Konsekuensi: **tidak ada posisi terbuka yang bisa teledor tanpa TP/SL yang sesuai konfigurasi**, termasuk posisi yang bukan dibuka oleh bot ini. `KILL SWITCH` membatalkan seluruh algo order pending sebelum berhenti.

## Fitur Manajemen Risiko (F1–F4)

Empat fitur tambahan untuk position sizing, proteksi harian, filter sesi, dan trailing stop — semuanya bisa diaktifkan/dinonaktifkan dari **Trade Configuration**.

### F1 — Position Sizing

- Toggle **On/Off** di panel konfigurasi.
- **On, mode `% Saldo`**: margin per trade = `Saldo × persen / 100`. Input harus ≤ 100%.
- **On, mode `Nominal`**: margin per trade = nilai USDT yang diinput. Tidak boleh melebihi saldo akun (ditolak dengan pesan error).
- **Off**: memakai `Margin per Trade (USDT)` biasa.
- Bila saldo belum diketahui saat startup (API balance belum tiba), mode `%` memakai fallback margin default agar order tidak gagal di awal.
- Koin yang margin-nya di bawah minimum 1 lot OKX otomatis *standby* (tidak di-order; dashboard menampilkan margin minimum yang dibutuhkan).

### F2 — Daily Loss Limit

- Toggle **On/Off**; mode **`%`** (default 5%) atau **Nominal USDT**.
- Loss harian = **realized** (jumlah `pnl` fill OKX sejak 00:00 UTC, termasuk fee) + **unrealized** (jumlah `upl` posisi terbuka saat ini).
- Basis mode `%` mengikuti **F1 secara dinamis**:
  - F1 **ON** → batas = hasil hitung Position Sizing × `% / 100`.
  - F1 **OFF** → batas = saldo akun saat ini × `% / 100`.
  - Mengubah status/mode F1 saat bot berjalan langsung mengubah basis di evaluasi berikutnya.
- Bila `Loss harian ≤ −batas` → **shutdown otomatis**: tutup semua posisi, batalkan order TP/SL pending, hentikan engine, dan **kunci start sampai 00:00 UTC** (naikkan/nonaktifkan batas untuk restart lebih awal).
- Counter di-reset otomatis setiap **00:00 UTC**. Semua perhitungan waktu memakai UTC; konversi ke WIB hanya untuk tampilan.

### F3 — Time Filter

- Dropdown sesi: `24/7` (default), `Asian (00:00–08:00 UTC)`, `London (08:00–16:00 UTC)`, `New York (13:00–21:00 UTC)`, `NY–London Overlap (13:00–16:00 UTC)`, atau `Custom` (input jam mulai/selesai, 0–23 UTC).
- Bot **hanya membuka posisi baru** di dalam jendela sesi (UTC).
- Posisi yang sudah terbuka **tetap dipantau dan dikelola** (TP/SL, trailing) meskipun waktu sudah keluar dari sesi.
- Jendela custom mendukung lintas tengah malam (mis. `22:00–02:00`).

### F4 — Trailing Stop (native OKX)

- Toggle **On/Off**; wajib input **Trigger Profit %** dan **Trailing Distance %**.
- **Dikerjakan sepenuhnya oleh OKX, bukan oleh server bot.** Satu order
  `ordType=move_order_stop` dipasang per posisi, lalu exchange yang menghitung
  dan menggeser trigger price-nya. Bot tidak pernah menghitung harga SL trailing
  dan tidak amends order secara berkala.
- Pemetaan konfigurasi → parameter OKX:

  | Konfigurasi | Parameter OKX | Arti |
  |---|---|---|
  | `TrailingDistance %` | `callbackRatio` | Jarak SL dari harga ekstrem (`0.3%` → `0.003`) |
  | `Trigger Profit %` | `activePx` | Harga aktivasi; sebelum tercapai trailing diam |

- Alur di sisi OKX:
  1. `activePx` belum tersentuh → order diam, SL fixed dari mode ATR/persen tetap melindungi.
  2. Harga mencapai `activePx` → OKX mulai melacak harga.
  3. Setelah aktif, trigger price = harga tertinggi sejak dipasang × `(1 − callbackRatio)` (LONG) atau harga terendah × `(1 + callbackRatio)` (SHORT).
  4. SL hanya bergerak menguntungkan — dijamin oleh exchange, tidak mungkin mundur.
- **Beban server:** saat konfigurasi tidak berubah, reconcile hanya **membaca**
  daftar order trailing (1 panggilan per siklus sinkronisasi untuk *semua*
  instrumen) dan **tidak melakukan panggilan mutasi apa pun**. Order baru hanya
  dibuat saat posisi muncul atau konfigurasi berubah, dengan cooldown 60 detik.
- **Toggle OFF** → semua order trailing yang menggantung dibatalkan, dan tidak
  ada order trailing baru yang dibuat.
- Order trailing memakai `reduceOnly`, jadi tidak pernah membuka posisi
  terbalik, dan aman berdampingan dengan TP/SL conditional yang sudah terpasang.
- Order trailing ikut dibatalkan saat posisi ditutup, saat emergency close, dan
  saat daily loss limit terpicu — supaya tidak memenuhi batas 5 pending
  trailing order per instrumen di OKX.
- **Trailing distance wajib > 0.17%** — di atas biaya round-trip OKX (~0.16%) — agar SL tidak tersentuh hanya karena fee.
- Harga trigger trailing yang dihitung OKX tampil di kartu koin (badge `TRAIL`,
  `MENUNGGU AKTIVASI` sebelum `activePx` tersentuh).

## Strategi Entry (Long/Short)

Strategi legacy berbasis tiga indikator ini dievaluasi **sekali per candle yang baru ditutup**, dan entry dipasang di **pembukaan candle berikutnya** memakai **Limit Order** (fee lebih rendah). Strategi dan gaya dapat dipilih dari panel Trade Configuration.

- Timeframe strategi legacy: `5m` (default).
- Indikator: **Bollinger Bands (20, 2)**, **RSI (14)** ambang 30/70, **EMA 200** sebagai filter tren utama.
- Buffer candle 250+ titik di-seed dari REST OKX saat startup agar semua indikator matang.

### Skenario LONG (uptrend + koreksi sementara)

1. Filter tren: **Close C2 > EMA 200** (uptrend utama).
2. Oversold: **RSI(14) < 30** pada candle sebelumnya (C1).
3. Konfirmasi reversal:
   - C1 sempat **menembus ke bawah Lower Band** (low C1 < LB C1).
   - C2 (candle terkini yang ditutup) **kembali tutup di atas Lower Band** (close C2 > LB C2).
4. Action: buka **Long** di pembukaan candle berikutnya (C3) dengan **Limit Order**.

### Skenario SHORT (downtrend + reli sementara)

1. Filter tren: **Close C2 < EMA 200** (downtrend utama).
2. Overbought: **RSI(14) > 70** pada candle sebelumnya (C1).
3. Konfirmasi reversal:
   - C1 sempat **menembus ke atas Upper Band** (high C1 > UB C1).
   - C2 **kembali tutup di bawah Upper Band** (close C2 < UB C2).
4. Action: buka **Short** di pembukaan candle berikutnya (C3) dengan **Limit Order**.

### Parameter eksekusi

- Entry memakai **Limit Order** di harga open C3; bila tidak terisi dalam **3 menit**, order dibatalkan otomatis (sinyal basi).
- Maks 1 sinyal per candle per koin + cooldown 5 menit (anti-spam).
- Maks 5 posisi bersamaan.
- TP/SL default: mode **ATR** (SL = ATR 14×1.0, TP = 2× SL → rasio 1:2); fallback persen `TP 0.8% / SL 0.4%`. TP/SL terpasang bersama entry via `attachAlgoOrds`.
- Margin isolated, leverage mengikuti input dashboard (`set-leverage` dipanggil sebelum tiap order).

Koin yang margin-nya tidak cukup untuk 1 lot tidak akan di-order; dashboard menampilkan nilai margin minimum yang dibutuhkan.

### Strategi tambahan (F5)

Default sistem adalah **MTF Confluence** dengan gaya **conservative**. Strategi legacy BB/RSI/EMA200 tetap tersedia untuk menjaga opsi perilaku entry sebelumnya.

| Strategi | Sinyal |
| --- | --- |
| **EMA + RSI** | EMA50 menjadi filter arah; entry saat RSI menyeberang kembali dari area oversold/overbought. |
| **MTF Confluence** | Arah tren EMA50 pada 1H, RSI pada 15m, dan persilangan MACD pada 5m harus searah. Candle ditarik dari endpoint market publik OKX dan hasilnya di-cache singkat. |
| **Advanced SMC + Volume** | Reaksi candle bullish/bearish dekat ekstrem 10 candle terakhir harus disertai lonjakan volume terhadap SMA20. |

Gaya **conservative** menambahkan filter ADX minimal 25 pada EMA+RSI dan MTF serta menggunakan ambang volume yang lebih tinggi pada Advanced SMC. Gaya **aggressive** melonggarkan ambang RSI atau jarak ekstrem. Sinyal semua strategi hanya menggunakan candle yang telah ditutup; entry tetap memakai LIMIT pada open candle berikutnya. Strategi baru sebaiknya diuji di mode demo sebelum digunakan live.

## Persyaratan

- Go versi 1.25 atau lebih baru, sesuai `go.mod`.
- Akun OKX dan API key untuk mode yang akan digunakan.
- Koneksi internet yang mengizinkan WebSocket ke OKX.
- Linux/macOS dengan shell kompatibel POSIX.

## Instalasi & Deploy

Panduan deploy lengkap — dari `git clone`, build, systemd service, firewall, nginx reverse proxy, sampai SSL dengan Certbot atau Cloudflare — ada di **[`INSTALLATION.md`](INSTALLATION.md)**.

Ringkasan cepat:

```sh
git clone https://github.com/mamashnl/OKX-Trading-Bot.git && cd OKX-Trading-Bot
cp .env.example .env && nano .env && chmod 600 .env
go build -o okx-bot .
systemctl enable --now okx-bot     # unit: /etc/systemd/system/okx-bot.service
```

Dashboard kemudian diakses lewat HTTPS domain (mis. `https://okx.maulmshr.my.id`) yang di-proxy nginx ke `127.0.0.1:8080`.

## Konfigurasi API

Aplikasi membaca `.env` dari direktori kerja saat startup.

```sh
cp .env.example .env
go build -o okx-bot .
./okx-bot
```

Perubahan `.env` hanya berlaku setelah restart. Jangan masukkan secret ke `main.go`, README, atau commit Git.

## Mode Demo

Default jika `OKX_MODE` kosong. Gunakan API key dari bagian **Demo Trading** OKX:

```dotenv
OKX_MODE=demo
OKX_DEMO_API_KEY=isi_api_key_demo
OKX_DEMO_SECRET_KEY=isi_secret_key_demo
OKX_DEMO_PASSPHRASE=isi_passphrase_demo
```

Mode ini memakai credential demo, header REST `x-simulated-trading: 1`, dan WebSocket demo `wss://wspap.okx.com:8443/ws/v5/business?brokerId=9999`.

> Environment demo OKX hanya mendukung sebagian instrumen. Koin di luar daftar itu akan ditolak WebSocket demo dengan `code 60018`; pesan tersebut ditampilkan di kartu koin pada dashboard.

## Beralih ke Live

**Live mengirim order ke akun sungguhan.** Gunakan API key live dengan izin minimum, jangan aktifkan withdrawal.

```dotenv
OKX_MODE=live
OKX_API_KEY=isi_api_key_live
OKX_SECRET_KEY=isi_secret_key_live
OKX_PASSPHRASE=isi_passphrase_live
```

Startup mencetak peringatan `LIVE TRADING MODE`. Mode tidak dapat diganti dari dashboard.

| `OKX_MODE` | REST order | WebSocket candle | Credential |
| --- | --- | --- | --- |
| `demo` atau kosong | `www.okx.com` + `x-simulated-trading: 1` | Demo business endpoint | `OKX_DEMO_*` |
| `live` | `www.okx.com` tanpa header simulasi | Production business endpoint | `OKX_API_*` |

## Dashboard

Server bind ke `0.0.0.0:8080`, lalu buka `http://<ip>:8080`.

### Panel Trade Configuration

Accordron yang tertutup secara default (tidak tergeser saat scroll). Isi:

1. **Margin per Trade (USDT)** — margin per posisi (dipakai saat F1 OFF).
2. **Leverage (1–125x)** — dipakai lewat `set-leverage` sebelum setiap order.
3. **Metode Penempatan SL/TP** — toggle **Persentase** / **ATR (Volatilitas)**.
   - Mode **Persentase**: input `Stop Loss (%)` dan `Take Profit (%)`.
   - Mode **ATR**: input `Periode ATR` (2–200, default 14) dan `SL = ATR × pengali` (0.1–5, default 1.0). Bidang persen tetap dikirim sebagai fallback.
4. **F1 Position Sizing** — toggle On/Off, mode `% Saldo` / `Nominal (USDT)`, dengan pratinjau margin efektif dan validasi (≤100% / ≤ saldo).
5. **F2 Daily Loss Limit** — toggle On/Off, mode `%` / `Nominal USDT`, status PnL harian + batas, dan indikator kunci engine saat limit tercapai.
6. **F3 Time Filter** — dropdown sesi + input jam custom (UTC) + chip status `SESI AKTIF` / `DI LUAR SESI`.
7. **F4 Trailing Stop** — toggle On/Off, input trigger & distance, catatan wajib distance > 0.17%. Trailing ditangani native OKX (`move_order_stop`); toggle Off langsung membatalkan order yang menggantung.
8. **5 dropdown instrumen** — 35 koin terverifikasi + `None`.

Kartu **Risk : Reward** dan **Break-even WR** di hero otomatis mengikuti mode (ATR → tetap `1 : 2.00`, WR 33.3%). Kartu koin menampilkan jarak TP/SL aktual per posisi (`tpDistPct`/`slDistPct`) beserta R:R per koin — di mode ATR jaraknya berbeda tiap posisi karena volatilitas berbeda. Hero juga menampilkan **Balance (USDT)** dan **PnL hari ini**; bila daily loss limit tercapai muncul banner merah peringatan.

Klik **Save Configuration** untuk menyimpan (accordion menutup otomatis) atau **START/STOP ENGINE**.

Nilai konfigurasi tidak akan di-reset oleh polling `/api/state` selama accordion sedang dibuka. Instrumen yang tidak tersedia di OKX ditolak dengan pesan error, bukan diam-diam diabaikan.

### Kartu koin

Menampilkan Order Size, Margin Used, Notional, Leverage, ukuran posisi, harga entry, TP, SL, dan badge `100% POSISI` bila TP/SL sudah terpasang di OKX.

## Akses dari HP (ngrok)

> **Sudah tidak diperlukan bila server punya domain publik.** Setup produksi saat ini memakai nginx + SSL sehingga dashboard dibuka langsung dari HP di `https://<domain-anda>`. Lihat [`INSTALLATION.md`](INSTALLATION.md). Cara ngrok di bawah hanya untuk server tanpa domain/port publik.

Karena port `8080` tidak dibuka ke publik, gunakan ngrok (jalankan dari direktori mana pun selama biner `ngrok` ada di `PATH`):

```sh
ngrok http 8080
```

URL yang ditampilkan dapat dibuka dari HP. URL gratis berubah setiap kali ngrok restart. Alternatif di jaringan lokal: `http://<ip-lan>:8080`.

## API

| Method | Endpoint | Keterangan |
| --- | --- | --- |
| `GET` | `/api/state` | Konfigurasi publik, state koin, log, TotalPnL, balance, dailyPnL, lossLimitHit. Secret tidak diserialisasi. |
| `POST` | `/api/config` | Ubah margin, leverage, 5 instrumen, status engine, timeframe, TP/SL, dan semua fitur F1–F4. |
| `POST` | `/api/emergency` | Hentikan engine, tutup 100% semua posisi, batalkan algo order. |

Contoh:

```sh
curl -X POST http://localhost:8080/api/config \
  -H 'Content-Type: application/json' \
  -d '{"margin":20,"leverage":20,"coins":["SOL-USDT-SWAP","DOGE-USDT-SWAP","XRP-USDT-SWAP","LINK-USDT-SWAP","ADA-USDT-SWAP","none","none","none","none","none"],"isRunning":true,"timeframe":"5m","tpSlMode":"atr","atrPeriod":14,"atrSlMult":1.0,"takeProfitPct":0.8,"stopLossPct":0.4,"posSizingEnabled":false,"posSizingMode":"pct","posSizingValue":5,"lossLimitEnabled":false,"lossLimitMode":"pct","lossLimitValue":5,"timeFilterMode":"24/7","customStartHour":0,"customEndHour":0,"trailingEnabled":false,"trailingTriggerPct":0.5,"trailingDistPct":0.3}'
```

Field TP/SL yang diterima `POST /api/config`: `tpSlMode` (`"percent"`/`"atr"`), `atrPeriod` (2–200), `atrSlMult` (≤ 5), `takeProfitPct`, `stopLossPct` (0–50%, TP ≥ SL, default 0.8/0.4).

Field fitur risiko (F1–F4):

- **F1**: `posSizingEnabled` (bool), `posSizingMode` (`"pct"`/`"fixed"`), `posSizingValue` (persen ≤ 100, atau nominal ≤ saldo).
- **F2**: `lossLimitEnabled` (bool), `lossLimitMode` (`"pct"`/`"fixed"`), `lossLimitValue` (> 0; maks 100 saat mode `%`).
- **F3**: `timeFilterMode` (`"24/7"`/`"asian"`/`"london"`/`"newyork"`/`"overlap"`/`"custom"`), `customStartHour`/`customEndHour` (0–23, hanya untuk `custom`).
- **F4**: `trailingEnabled` (bool), `trailingTriggerPct` (> 0), `trailingDistPct` (> 0.17, maks 20).

Menyimpan config dengan `isRunning: true` ditolak (HTTP 400) bila daily loss limit masih tercapai — restart hanya bisa dilakukan setelah 00:00 UTC, atau dengan menaikkan/menonaktifkan batas.

## Validasi

```sh
gofmt -w main.go
go vet ./...
go build -o okx-bot .
go build -race -o okx-bot-race .   # verifikasi tidak ada data race
```
