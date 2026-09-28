# OKX Scalper Bot

Bot Go dengan dashboard lokal untuk memantau candle swap USDT OKX dan menjalankan strategi Bollinger Bands + RSI. Mode awal adalah **demo**. Mode live tersedia, tetapi mengirim order sungguhan dan berisiko kehilangan dana.

> **Peringatan:** Jangan beralih ke live sebelum menguji demo dan memverifikasi ukuran kontrak, parameter order, leverage, serta status posisi di akun OKX. Bot menyimpan posisi hanya di memori lokal dan tidak merekonsiliasi posisi akun saat restart. Nilai PnL pada dashboard belum dihitung secara live. Ini bukan sistem trading siap produksi.

## Persyaratan

- Go versi 1.25 atau lebih baru, sesuai `go.mod`.
- Akun OKX dan API key untuk mode yang akan digunakan.
- Koneksi internet yang mengizinkan koneksi WebSocket ke OKX.
- Linux/macOS dengan shell kompatibel POSIX untuk contoh perintah di bawah.

## Konfigurasi API

Aplikasi membaca environment variables saat startup. File `.env` **tidak dibaca otomatis**; muat variabelnya ke shell sebelum menjalankan aplikasi.

Jika file `.env` belum ada, buat dari contoh:

```sh
cp .env.example .env
```

Jika `.env` sudah ada, jangan menimpanya. Tambahkan atau ubah variabel yang diperlukan di file tersebut. Jangan masukkan secret ke `main.go`, README, atau commit Git.

Muat variabel dan jalankan bot dari direktori proyek:

```sh
set -a
. ./.env
set +a
go run .
```

Untuk build dan menjalankan binary:

```sh
go build -o okx-bot .
./okx-bot
```

Perubahan pada `.env` hanya berlaku setelah proses bot dihentikan dan dimulai ulang.

## Mode Demo

Demo adalah default jika `OKX_MODE` tidak ditentukan. Buat API key dari bagian **Demo Trading** OKX, bukan dari akun live. Isi variabel demo berikut di `.env`:

```dotenv
OKX_MODE=demo
OKX_DEMO_API_KEY=isi_api_key_demo
OKX_DEMO_SECRET_KEY=isi_secret_key_demo
OKX_DEMO_PASSPHRASE=isi_passphrase_demo
```

Mode ini memilih credential demo, menambahkan header REST `x-simulated-trading: 1`, dan menggunakan WebSocket demo OKX untuk candle. Credential live tidak dipakai sebagai fallback.

## Beralih ke Live

**Live mengirim order ke akun sungguhan.** Pastikan saldo, margin, ukuran kontrak, leverage, dan risiko sudah diverifikasi. Buat dan gunakan API key live dengan izin minimum yang diperlukan; jangan aktifkan izin withdrawal.

Ubah `.env` menjadi:

```dotenv
OKX_MODE=live
OKX_API_KEY=isi_api_key_live
OKX_SECRET_KEY=isi_secret_key_live
OKX_PASSPHRASE=isi_passphrase_live
```

Mode live menggunakan API REST dan WebSocket production, serta tidak mengirim header simulasi. Hentikan bot, muat ulang `.env`, lalu jalankan lagi. Startup akan mencetak peringatan `LIVE TRADING MODE` dan dashboard menampilkan `OKX Live Trading • Real Orders`.

## Kembali ke Demo

Ubah `.env` kembali menjadi:

```dotenv
OKX_MODE=demo
OKX_DEMO_API_KEY=isi_api_key_demo
OKX_DEMO_SECRET_KEY=isi_secret_key_demo
OKX_DEMO_PASSPHRASE=isi_passphrase_demo
```

Lalu hentikan dan mulai ulang proses. Bot kembali memakai endpoint dan credential demo; credential live tidak digunakan.

| `OKX_MODE` | REST order | WebSocket candle | Credential |
| --- | --- | --- | --- |
| `demo` atau kosong | `www.okx.com` dengan `x-simulated-trading: 1` | Demo business endpoint | `OKX_DEMO_*` |
| `live` | `www.okx.com` tanpa header simulasi | Production business endpoint | `OKX_API_KEY`, `OKX_SECRET_KEY`, `OKX_PASSPHRASE` |

Mode tidak dapat diganti dari dashboard; ubah environment variable dan restart aplikasi. Nilai selain `demo` atau `live` membuat aplikasi berhenti saat startup.

## Menjalankan dan Menggunakan Dashboard

Jalankan `go run .`, lalu buka <http://127.0.0.1:8080>. Server hanya bind ke localhost.

Dashboard menampilkan harga, status koneksi/log, instrumen, ukuran kontrak hasil kalkulasi, dan status posisi lokal. Feed candle berjalan saat aplikasi hidup, termasuk saat strategi STOP. Tombol START mengaktifkan evaluasi sinyal dan dapat mengirim order sesuai mode; di live, order memakai dana sungguhan. Tombol KILL SWITCH menghentikan strategi dan mencoba menutup posisi yang diketahui oleh state lokal.

API lokal yang tersedia:

- `GET /api/state` mengembalikan konfigurasi publik, state koin, dan log. Secret API tidak diserialisasi.
- `POST /api/config` mengubah margin, leverage, instrumen, status strategi, dan timeframe untuk proses berjalan.
- `POST /api/emergency` menghentikan strategi dan mencoba menutup posisi lokal.

## Strategi Saat Ini

- Timeframe default: `5m`.
- Sinyal BUY: harga menyentuh area Bollinger Band bawah dan RSI di bawah 30.
- Sinyal SELL: harga menyentuh area Bollinger Band atas dan RSI di atas 70.
- Bot menunggu setidaknya 20 sampel harga sebelum menghitung indikator.
- Target take profit dan stop loss di kode saat ini masing-masing sekitar 0,3% dan 0,5% dari harga masuk.
- Order menggunakan market order dan mode margin isolated.

Ini adalah strategi dan implementasi sederhana, bukan jaminan profit. Kontrak OKX dapat memiliki `ctVal`, `ctMult`, `lotSz`, dan `minSz` berbeda. Kode saat ini memakai peta multiplier statis untuk estimasi ukuran; cocokkan kalkulasi itu dengan spesifikasi instrumen OKX sebelum trading live. State posisi juga hanya lokal, sehingga restart dapat membuat state bot berbeda dari posisi akun.

## Validasi

```sh
go test ./...
go test -race ./...
go vet ./...
```

Saat ini paket belum memiliki test otomatis; perintah tersebut terutama memeriksa kompilasi, race yang teramati selama test, dan masalah statis.

## Keamanan Credential

- `.env` diabaikan Git dan tidak boleh dibagikan.
- Buat credential demo dan live secara terpisah.
- Cabut dan rotasi credential yang pernah tertanam atau dibagikan di source/chat.
- Jangan mengaktifkan withdrawal pada API key bot.
- Jalankan dashboard hanya pada mesin tepercaya; jangan expose port `8080` ke internet.
