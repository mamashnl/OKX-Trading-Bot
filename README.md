# OKX Scalper Bot

Bot Go dengan dashboard web responsif untuk memantau candle swap USDT OKX dan menjalankan strategi Bollinger Bands + RSI. Defaultnya **demo trading**. Mode live tersedia, tetapi mengirim order sungguhan dan berisiko kehilangan dana.

> **Peringatan:** Jangan beralih ke live sebelum menguji demo dan memverifikasi ukuran kontrak, parameter order, leverage, serta status posisi di akun OKX. Ini bukan sistem trading siap produksi.

## Daftar Isi

- [Fitur Utama](#fitur-utama)
- [Perhitungan Margin & Ukuran Order](#perhitungan-margin--ukuran-order)
- [TP/SL Entire Position](#tpsl-entire-position)
- [Strategi](#strategi)
- [Persyaratan](#persyaratan)
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

| Koin | Hardcode (lama) | `ctVal` OKX (benar) | Error |
| --- | --- | --- | --- |
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
hadiah yang terjadi   >= Take Profit%   (dibulatkan menjauh dari entry)
risiko yang terjadi   <= Stop Loss%     (dibulatkan mendekati entry)
R:R yang terjadi      >= 1 : 2          (tidak pernah lebih buruk dari config)
```

Contoh dengan tick size asli (ADA, entry 0.244786, tick 0.0001): TP dibulatkan ke atas → `+0.823%` (≥ 0.8%), SL dibulatkan ke atas juga → `−0.362%` (≤ 0.4%). Risiko tidak pernah melebihi yang dikonfigurasi.

### Kenapa harus dua elemen terpisah

Jika `tpTriggerPx` dan `slTriggerPx` diletakkan pada **satu** elemen `attachAlgoOrds`, OKX akan **membagi ukuran posisi** (setengah untuk TP, setengah untuk SL). Dua elemen terpisah memastikan masing-masing menutup 100%.

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

## Strategi

- Timeframe default: `5m`.
- Sinyal BUY: harga menyentuh area Bollinger Band bawah dan RSI di bawah 30.
- Sinyal SELL: harga menyentuh area Bollinger Band atas dan RSI di atas 70.
- Minimal 10 sampel harga sebelum indikator dihitung (riwayat di-seed dari REST saat startup).
- Cooldown 5 menit per koin antar sinyal.
- Maks 5 posisi bersamaan.
- TP/SL default: mode **ATR** (SL = ATR 14×1.0, TP = 2× SL → rasio 1:2); fallback persen `TP 0.8% / SL 0.4%`.
- Market order, margin isolated, leverage mengikuti input dashboard (`set-leverage` dipanggil sebelum tiap order).

Koin yang margin-nya tidak cukup untuk 1 lot tidak akan di-order; dashboard menampilkan nilai margin minimum yang dibutuhkan.

## Persyaratan

- Go versi 1.25 atau lebih baru, sesuai `go.mod`.
- Akun OKX dan API key untuk mode yang akan digunakan.
- Koneksi internet yang mengizinkan WebSocket ke OKX.
- Linux/macOS dengan shell kompatibel POSIX.

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

1. **Margin per Trade (USDT)** — margin per posisi.
2. **Leverage (1–125x)** — dipakai lewat `set-leverage` sebelum setiap order.
3. **Metode Penempatan SL/TP** — toggle **Persentase** / **ATR (Volatilitas)**.
   - Mode **Persentase**: input `Stop Loss (%)` dan `Take Profit (%)`.
   - Mode **ATR**: input `Periode ATR` (2–200, default 14) dan `SL = ATR × pengali` (0.1–5, default 1.0). Bidang persen tetap dikirim sebagai fallback.
4. **5 dropdown instrumen** — 35 koin terverifikasi + `None`.

Kartu **Risk : Reward** dan **Break-even WR** di hero otomatis mengikuti mode (ATR → tetap `1 : 2.00`, WR 33.3%). Kartu koin menampilkan jarak TP/SL aktual per posisi (`tpDistPct`/`slDistPct`) beserta R:R per koin — di mode ATR jaraknya berbeda tiap posisi karena volatilitas berbeda.

Klik **Save Configuration** untuk menyimpan (accordion menutup otomatis) atau **START/STOP ENGINE**.

Nilai konfigurasi tidak akan di-reset oleh polling `/api/state` selama accordion sedang dibuka. Instrumen yang tidak tersedia di OKX ditolak dengan pesan error, bukan diam-diam diabaikan.

### Kartu koin

Menampilkan Order Size, Margin Used, Notional, Leverage, ukuran posisi, harga entry, TP, SL, dan badge `100% POSISI` bila TP/SL sudah terpasang di OKX.

## Akses dari HP (ngrok)

Karena port `8080` tidak dibuka ke publik, gunakan ngrok (jalankan dari direktori mana pun selama biner `ngrok` ada di `PATH`):

```sh
ngrok http 8080
```

URL yang ditampilkan dapat dibuka dari HP. URL gratis berubah setiap kali ngrok restart. Alternatif di jaringan lokal: `http://<ip-lan>:8080`.

## API

| Method | Endpoint | Keterangan |
| --- | --- | --- |
| `GET` | `/api/state` | Konfigurasi publik, state koin, log, TotalPnL. Secret tidak diserialisasi. |
| `POST` | `/api/config` | Ubah margin, leverage, 5 instrumen, status engine, timeframe. |
| `POST` | `/api/emergency` | Hentikan engine, tutup 100% semua posisi, batalkan algo order. |

Contoh:

```sh
curl -X POST http://localhost:8080/api/config \
  -H 'Content-Type: application/json' \
  -d '{"margin":20,"leverage":20,"coins":["SOL-USDT-SWAP","DOGE-USDT-SWAP","XRP-USDT-SWAP","LINK-USDT-SWAP","ADA-USDT-SWAP","none","none","none","none","none"],"isRunning":true,"timeframe":"5m","tpSlMode":"atr","atrPeriod":14,"atrSlMult":1.0,"takeProfitPct":0.8,"stopLossPct":0.4}'
```

Field TP/SL yang diterima `POST /api/config`: `tpSlMode` (`"percent"`/`"atr"`), `atrPeriod` (2–200), `atrSlMult` (≤ 5), `takeProfitPct`, `stopLossPct` (0–50%, TP ≥ SL, default 0.8/0.4).

## Validasi

```sh
gofmt -w main.go
go vet ./...
go build -o okx-bot .
go build -race -o okx-bot-race .   # verifikasi tidak ada data race
```

Paket belum memiliki test otomatis tersimpan; `go test` hanya memeriksa kompilasi. Verifikasi yang sudah dilakukan:

- **Sizing**: 11 koin × 6 kombinasi margin/leverage, semua `size` valid kelipatan `lotSz`.
- **Entry live OKX demo**: order + TP + SL, ketiganya `sz` identik dengan ukuran posisi.
- **Arah TP/SL short**: TP di bawah harga masuk, SL di atas.
- **`ensureTPSL` idempoten**: pemanggilan kedua tidak membuat order duplikat.
- **Full close**: `close-position` menghasilkan posisi flat.
- **Race detector**: 0 `DATA RACE` selama >90 detik operasi live.
- **ATR (unit test sementara, sudah dihapus)**: nilai ATR dari OHLC sintetis, jarak SL = ATR×mult, TP = 2× SL, invariant pembulatan (risiko ≤ diminta, hadiah ≥ diminta, R:R ≥ 2), fallback persen tanpa data, clamp min 0.25% & cap anti-likuidasi.
- **Mode ATR live OKX demo**: level SL = ATR×1.0, TP = 2× jarak SL (R:R 1:2.00), tepat 1 TP + 1 SL per posisi 100% `reduceOnly`, dan watchdog tidak mengubah level di sinkronisasi berikutnya (anti-churn).

## Keamanan Credential

- `.env` diabaikan Git (lihat `.gitignore`) dan tidak boleh di-commit atau dibagikan. Riwayat repo ini telah diaudit — **tidak ada secret dalam riwayat commit**.
- Credential demo dan live dibuat secara terpisah; beri izin API key seminimal mungkin (trade saja), jangan aktifkan withdrawal.
- **Rotasi credential segera** jika pernah terekspos di chat, screenshot, atau dokumen lain yang tidak seharusnya.
- Batasi akses dashboard; gunakan ngrok dengan hati-hati (URL publik bersifat sementara).
