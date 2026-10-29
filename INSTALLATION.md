# INSTALLATION.md — Panduan Deploy OKX Scalper Bot

Panduan lengkap dari server kosong sampai dashboard bisa dibuka lewat domain HTTPS.
Ditulis berdasarkan kondisi nyata server produksi (`Ubuntu 24.04.5 LTS`, Go 1.25.4, nginx 1.24.0,
certbot 2.9.0, domain `okx.maulmshr.my.id`).

> **Peringatan risiko:** bot ini mengirim order ke bursa OKX. Selesaikan seluruh instalasi dengan
> `OKX_MODE=demo` dan verifikasi perilakunya sebelum mempertimbangkan mode `live`. Mode live
> memakai dana sungguhan dan bisa rugi.

## Daftar Isi

- [Arsitektur](#arsitektur)
- [Prasyarat](#prasyarat)
- [1. Instal Go](#1-instal-go)
- [2. Clone & Build](#2-clone--build)
- [3. Konfigurasi `.env`](#3-konfigurasi-env)
- [4. Uji Jalan Lokal](#4-uji-jalan-lokal)
- [5. systemd Service](#5-systemd-service)
- [6. Firewall (ufw)](#6-firewall-ufw)
- [7. nginx Reverse Proxy](#7-nginx-reverse-proxy)
- [8. DNS & SSL](#8-dns--ssl)
  - [Opsi A — DNS biasa + Certbot (dipakai server ini)](#opsi-a--dns-biasa--certbot-dipakai-server-ini)
  - [Opsi B — Cloudflare](#opsi-b--cloudflare)
- [9. Verifikasi Akhir](#9-verifikasi-akhir)
- [10. Operasional Sehari-hari](#10-operasional-sehari-hari)
- [11. Troubleshooting](#11-troubleshooting)
- [12. Checklist Keamanan](#12-checklist-keamanan)

## Arsitektur

```
                        ┌──────────────────────────────────────────┐
 Browser / HP ──HTTPS──▶│ nginx :443 (SSL terminator)              │
   443                  │   location / → proxy_pass 127.0.0.1:8080 │
                        └───────────────────┬──────────────────────┘
                                            │ HTTP loopback
                        ┌───────────────────▼──────────────────────┐
                        │ okx-bot (systemd) :8080                  │
                        │   /            → dashboard HTML          │
                        │   /api/state   → GET  state & log        │
                        │   /api/config  → POST ubah konfigurasi   │
                        │   /api/emergency → POST kill switch      │
                        └───────────────────┬──────────────────────┘
                                            │ outbound
                                   REST + WebSocket OKX
                              (www.okx.com / wspap.okx.com:8443)
```

Poin kunci:

- Semua koneksi WebSocket adalah **bot → OKX (outbound)**. Dashboard di browser hanya
  polling HTTP ke `/api/state`, jadi tidak butuh endpoint WebSocket khusus. Header
  `Upgrade`/`Connection` di nginx tetap dipasang agar aman bila nanti dipakai.
- Port `8080` di-bind ke `0.0.0.0`, tetapi **tidak dibuka di firewall** — akses publik
  hanya lewat nginx `:443`. Lihat [bagian 6](#6-firewall-ufw).
- Bot membaca `.env` dari **direktori kerja saat start**, bukan dari lokasi binary.
  Karena itu `WorkingDirectory` di systemd wajib diisi benar.

## Prasyarat

| Kebutuhan | Nilai di server ini | Catatan |
| --- | --- | --- |
| OS | Ubuntu 24.04.5 LTS | Debian/Ubuntu lain umumnya sama |
| Go | 1.25.4 | `go.mod` mensyaratkan ≥ 1.25.4 — lihat [bagian 1](#1-instal-go) |
| RAM | ≥ 1 GB cukup | Bot ringan (~10 MB RSS) |
| Disk | ≥ 500 MB | Untuk Go toolchain + module cache |
| Akses | root / sudo | |
| Domain | `okx.maulmshr.my.id` | A record → IP publik server |
| Jaringan | Outbound 443 & 8443 | Ke `www.okx.com` dan `wspap.okx.com` |

Cek cepat:

```sh
. /etc/os-release && echo "$PRETTY_NAME"
curl -s ifconfig.me && echo        # IP publik server
ss -tlnp | grep -E ':80|:443|:8080'
```

## 1. Instal Go

**Jebakan:** Go bawaan `apt` Ubuntu 24.04 adalah **1.22.2**, lebih tua dari syarat `go.mod`
(`go 1.25.4`). Ada dua jalan:

### Cara A — Biarkan Go mengunduh toolchain sendiri (paling mudah)

Cukup pasang Go dari apt; karena `GOTOOLCHAIN=auto` (default), Go otomatis mengunduh
toolchain 1.25.4 ke module cache saat build pertama. Butuh internet dan cache yang bisa ditulis.

```sh
apt update && apt install -y golang-go git
go env GOTOOLCHAIN     # harus: auto
go version             # setelah build pertama akan melaporkan 1.25.4
```

Ini persis yang terjadi di server produksi: `GOTOOLCHAIN=local go version` = `go1.22.2`,
tetapi `go version` biasa = `go1.25.4` (toolchain di `~/go/pkg/mod/golang.org/toolchain@…`).

### Cara B — Instal Go 1.25 resmi (rekomendasi untuk server baru)

Lebih cepat dan deterministik, tidak bergantung unduhan toolchain tiap kali:

```sh
curl -LO https://go.dev/dl/go1.25.4.linux-amd64.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf go1.25.4.linux-amd64.tar.gz
ln -sf /usr/local/go/bin/go /usr/local/bin/go
go version             # go version go1.25.4 linux/amd64
```

## 2. Clone & Build

```sh
cd /root
git clone https://github.com/mamashnl/OKX-Trading-Bot.git
cd OKX-Trading-Bot

gofmt -l .                 # tidak boleh ada output
go vet ./...               # tidak boleh ada output
go build -o okx-bot .      # hasil: ./okx-bot
```

Opsional, verifikasi bebas data race (build lebih lambat):

```sh
go build -race -o okx-bot-race .
```

Struktur akhir direktori:

```
/root/OKX-Trading-Bot/
├── .env            ← credential, JANGAN di-commit
├── .env.example
├── okx-bot         ← binary hasil build
├── main.go
└── README.md
```

`.gitignore` sudah menutup `.env`, `okx-bot`, `okx-bot-race`, `output.log`, dan `race.log`.
Jangan jalankan bot dengan `nohup … > output.log` — log harus masuk journald (lihat [bagian 5](#5-systemd-service)).

## 3. Konfigurasi `.env`

```sh
cp .env.example .env
nano .env
chmod 600 .env        # hanya owner yang bisa baca
```

Isi sesuai mode:

```dotenv
OKX_MODE=demo

# Dipakai saat OKX_MODE=demo — buat di OKX → Account → API → Demo Trading
OKX_DEMO_API_KEY=...
OKX_DEMO_SECRET_KEY=...
OKX_DEMO_PASSPHRASE=...

# Dipakai saat OKX_MODE=live — izin trade saja, TANPA withdrawal
OKX_API_KEY=...
OKX_SECRET_KEY=...
OKX_PASSPHRASE=...
```

| `OKX_MODE` | REST | WebSocket | Credential |
| --- | --- | --- | --- |
| `demo` / kosong | `www.okx.com` + header `x-simulated-trading: 1` | `wspap.okx.com:8443` (demo business) | `OKX_DEMO_*` |
| `live` | `www.okx.com` | production business endpoint | `OKX_API_*` |

Perubahan `.env` baru berlaku setelah `systemctl restart okx-bot`.

> Environment demo OKX hanya mendukung sebagian instrumen. Koin di luar daftar ditolak
> WebSocket demo dengan `code 60018`; pesannya tampil di kartu koin pada dashboard.

## 4. Uji Jalan Lokal

```sh
./okx-bot
```

Startup yang sehat mencetak:

```
OKX demo trading mode enabled.
[SYSTEM] Trading mode: DEMO
[SPEC] Loaded 526 OKX instrument specs (ctVal/lotSz/tickSize)
[CANDLE HISTORY] BTC-USDT-SWAP: 250 candle 5m dimuat
[SYNC] Positions and PnL synced with OKX
[WS CONNECTED] BTC-USDT-SWAP berhasil terhubung.
========================================
  OKX SCALPER PRO (DEMO MODE)
  UI Ready at: http://localhost:8080
========================================
```

Uji endpoint dari shell lain:

```sh
curl -s http://127.0.0.1:8080/api/state | head -c 300
```

Harus mengembalikan JSON berisi `"mode":"demo"` dan `"isRunning":false`.
Tekan `Ctrl-C` untuk berhenti, lalu lanjut ke systemd.

> Bila muncul `listen tcp 0.0.0.0:8080: bind: address already in use`, ada proses lama
> yang masih hidup: `pgrep -af okx-bot` lalu `systemctl stop okx-bot` / `kill <PID>`.

## 5. systemd Service

Buat `/etc/systemd/system/okx-bot.service`:

```ini
[Unit]
Description=OKX Scalper Trading Bot (dashboard :8080)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/root/OKX-Trading-Bot
ExecStart=/root/OKX-Trading-Bot/okx-bot
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

`WorkingDirectory` **wajib menunjuk** direktori repo karena `godotenv.Load()` membaca `.env`
dari direktori kerja saat proses start.

Aktifkan:

```sh
systemctl daemon-reload
systemctl enable --now okx-bot
systemctl status okx-bot --no-pager
journalctl -u okx-bot -n 30 --no-pager
```

`enabled` berarti bot ikut start otomatis setelah server reboot; `Restart=on-failure`
menyalakan ulang bila proses crash (jeda 5 detik).

Catatan: engine trading **tidak** auto-start. Setelah restart, `isRunning` kembali `false`
dan harus di-START dari dashboard. Ini disengaja agar tidak ada order tak terduga pasca-reboot.

## 6. Firewall (ufw)

Aturan di server produksi:

```
Default: deny (incoming), allow (outgoing)

To                         Action      From
OpenSSH                    ALLOW       Anywhere
Nginx Full                 ALLOW       Anywhere
80/tcp                     ALLOW       Anywhere
443/tcp                    ALLOW       Anywhere
```

**Port 8080 sengaja tidak dibuka.** Bot tetap bisa diakses publik melalui nginx, sedangkan
`http://<ip-server>:8080` dari internet akan timeout. Ini memaksa semua trafik lewat HTTPS.

Jika belum ada aturannya:

```sh
ufw allow OpenSSH
ufw allow 'Nginx Full'      # = 80 + 443
ufw enable
ufw status numbered
```

Jangan `ufw allow 8080` kecuali sedang debugging dari jaringan lokal, dan hapus lagi sesudahnya:

```sh
ufw allow from 192.168.1.0/24 to any port 8080 proto tcp   # contoh debugging LAN
ufw delete allow 8080/tcp
```

Outbound ke OKX (`443`, `8443`) sudah diizinkan karena default policy outgoing = allow.

## 7. nginx Reverse Proxy

Pasang nginx bila belum ada:

```sh
apt install -y nginx
```

Buat site **terpisah** untuk bot: `/etc/nginx/sites-available/okx.maulmshr.my.id`
(ganti nama domain sesuai punya Anda).

```nginx
server {
    server_name okx.maulmshr.my.id;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }

    listen [::]:443 ssl ipv6only=on; # managed by Certbot
    listen 443 ssl; # managed by Certbot
    ssl_certificate /etc/letsencrypt/live/okx.maulmshr.my.id/fullchain.pem; # managed by Certbot
    ssl_certificate_key /etc/letsencrypt/live/okx.maulmshr.my.id/privkey.pem; # managed by Certbot
    include /etc/letsencrypt/options-ssl-nginx.conf; # managed by Certbot
    ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem; # managed by Certbot
}

server {
    if ($host = okx.maulmshr.my.id) {
        return 301 https://$host$request_uri;
    } # managed by Certbot

    listen 80;
    listen [::]:80;
    server_name okx.maulmshr.my.id;
    return 404; # managed by Certbot
}
```

Baris bertanda `# managed by Certbot` ditambahkan otomatis oleh certbot — pada instalasi
baru Anda cukup menulis bagian `server_name`, `location /`, dan `listen 80`, lalu jalankan
certbot yang akan menyisipkan blok SSL sendiri (lihat [bagian 8](#8-dns--ssl)).

Versi minimal sebelum certbot:

```nginx
server {
    listen 80;
    listen [::]:80;
    server_name okx.maulmshr.my.id;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

Aktifkan dan uji:

```sh
ln -sf /etc/nginx/sites-available/okx.maulmshr.my.id /etc/nginx/sites-enabled/okx.maulmshr.my.id
nginx -t && systemctl reload nginx
```

`/etc/nginx/nginx.conf` Ubuntu sudah memuat `include /etc/nginx/sites-enabled/*;` sehingga
tidak perlu edit `nginx.conf`.

**Penting:** jangan tinggalkan blok `location / { try_files $uri $uri/ =404; }` untuk
`server_name` domain bot. Itu penyebab umum dashboard tetap menampilkan halaman default
nginx meski bot sudah jalan. Pastikan juga tidak ada dua `server` block dengan
`server_name` sama.

## 8. DNS & SSL

### Opsi A — DNS biasa + Certbot (dipakai server ini)

Kondisi server produksi sekarang: nameserver domain `orbit.dns-parking.com` /
`horizon.dns-parking.com` (parking Hostinger/RumahWeb), A record langsung ke IP server,
sertifikat Let's Encrypt via certbot.

**1. Arahkan DNS** di panel registrar/hosting:

| Type | Name | Value | TTL |
| --- | --- | --- | --- |
| A | `okx` | `172.232.242.208` | Auto |
| AAAA | `okx` | `2600:3c0c::2000:70ff:fe09:ad89` | Auto (opsional) |

Tunggu propagasi, lalu pastikan sudah menunjuk IP server:

```sh
dig +short okx.maulmshr.my.id        # harus = IP publik server
```

**2. Terbitkan sertifikat** (butuh port 80 terbuka dan DNS sudah benar):

```sh
apt install -y certbot python3-certbot-nginx
certbot --nginx -d okx.maulmshr.my.id \
  --redirect --agree-tos -m anda@email.com --no-eff-email
```

Certbot akan otomatis menambahkan blok `listen 443 ssl`, path sertifikat, dan redirect
HTTP→HTTPS ke file site nginx, lalu reload nginx.

**3. Renewal otomatis** sudah dipasang oleh paket certbot sebagai systemd timer:

```sh
systemctl list-timers | grep certbot
certbot renew --dry-run
```

Sertifikat berlaku 90 hari (server ini: terbit 2026-10-28, kedaluwarsa 2027-01-26) dan
diperpanjang otomatis menjelang expiry.

### Opsi B — Cloudflare

Cloudflare dipakai bila ingin **menyembunyikan IP origin**, mendapat proteksi DDoS/WAF,
dan manajemen DNS terpusat. Ada dua varian; pilih salah satu.

#### B0. Pindahkan DNS ke Cloudflare (langkah awal kedua varian)

1. Daftar/login di [dash.cloudflare.com](https://dash.cloudflare.com) → **Add a site** → `maulmshr.my.id`.
2. Cloudflare memindai record yang ada. Pastikan record `A okx → <IP server>` terbawa.
3. Cloudflare memberi dua nameserver, mis. `xxx.ns.cloudflare.com` dan `yyy.ns.cloudflare.com`.
4. Di panel registrar, **ganti nameserver** dari `orbit/horizon.dns-parking.com` ke nameserver Cloudflare.
5. Tunggu status **Active** (propagasi beberapa menit sampai 24 jam):

```sh
dig +short NS maulmshr.my.id     # harus berubah ke *.ns.cloudflare.com
```

> Mengganti nameserver memindahkan **seluruh** kendali DNS zona itu ke Cloudflare.
> Record lain (mail, subdomain lain) harus disalin dulu agar tidak putus.

#### B1. DNS-only (grey cloud) + Certbot — paling sederhana

Cloudflare hanya jadi resolver; trafik langsung ke server. Sertifikat tetap Let's Encrypt.

1. Di **DNS → Records**, record `A okx` disetel **DNS only** (ikon awan abu-abu).
2. Ikuti [Opsi A](#opsi-a--dns-biasa--certbot-dipakai-server-ini) persis seperti biasa.
3. Selesai. Config nginx tidak perlu diubah.

Kelebihan: certbot HTTP-01 selalu jalan, tidak ada risiko redirect loop.
Kekurangan: IP origin tetap terlihat lewat `dig`, tidak ada WAF/cache.

#### B2. Proxied (orange cloud) + Cloudflare Origin Certificate — IP origin tersembunyi

Cloudflare jadi reverse proxy di depan nginx. Browser ↔ Cloudflare (cert Cloudflare),
Cloudflare ↔ nginx (cert Origin CA).

**1. Setel record ke Proxied**

Di **DNS → Records**, record `A okx` → **Proxied** (awan oranye). Setelah ini
`dig +short okx.maulmshr.my.id` akan mengembalikan **IP Cloudflare**, bukan IP server — itu benar.

**2. Wajib: mode SSL/TLS = Full (strict)**

Di **SSL/TLS → Overview**, pilih **Full (strict)**.

| Mode | CF → origin | Hasil dengan config nginx kita |
| --- | --- | --- |
| Off | HTTP | Tidak aman, jangan dipakai |
| Flexible | HTTP:80 | **Redirect loop** — nginx kita 301 ke HTTPS |
| Full | HTTPS, cert apa saja | Jalan, tapi cert origin tidak diverifikasi |
| **Full (strict)** | HTTPS, cert valid | **Dipakai** — Let's Encrypt atau Origin CA |

**3. Buat Origin Certificate**

**SSL/TLS → Origin Server → Create Certificate**:

- Hostnames: `okx.maulmshr.my.id` dan `*.maulmshr.my.id` (bila perlu)
- Validity: 15 tahun
- Simpan **Certificate** dan **Private key** (private key hanya muncul sekali)

```sh
mkdir -p /etc/ssl/cloudflare
nano /etc/ssl/cloudflare/okx.maulmshr.my.id.pem    # tempel Certificate
nano /etc/ssl/cloudflare/okx.maulmshr.my.id.key    # tempel Private key
chmod 600 /etc/ssl/cloudflare/okx.maulmshr.my.id.key
```

**4. Ubah site nginx** agar memakai cert Origin CA (ganti empat baris `# managed by Certbot`
untuk `ssl_certificate`, `ssl_certificate_key`, `include options-ssl-nginx.conf`, `ssl_dhparam`):

```nginx
server {
    server_name okx.maulmshr.my.id;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }

    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    ssl_certificate     /etc/ssl/cloudflare/okx.maulmshr.my.id.pem;
    ssl_certificate_key /etc/ssl/cloudflare/okx.maulmshr.my.id.key;
    ssl_protocols TLSv1.2 TLSv1.3;
}

server {
    listen 80;
    listen [::]:80;
    server_name okx.maulmshr.my.id;
    return 301 https://$host$request_uri;
}
```

```sh
nginx -t && systemctl reload nginx
```

> Setelah pindah ke Origin CA, **hentikan certbot memperpanjang cert untuk domain ini**
> agar tidak saling menimpa: `certbot delete --cert-name okx.maulmshr.my.id`.
> Origin CA berlaku 15 tahun dan tidak butuh renewal.

**5. IP klien asli di log**

Di belakang proxy Cloudflare, `$remote_addr` = IP Cloudflare. Ambil IP asli dari header
`CF-Connecting-IP` memakai modul `ngx_http_realip_module` (sudah termasuk di nginx Ubuntu).
Buat `/etc/nginx/conf.d/cloudflare-realip.conf`:

```nginx
# Ambil dari https://www.cloudflare.com/ips/
set_real_ip_from 173.245.48.0/20;
set_real_ip_from 103.21.244.0/22;
set_real_ip_from 103.22.200.0/22;
set_real_ip_from 103.31.4.0/22;
set_real_ip_from 141.101.64.0/18;
set_real_ip_from 108.162.192.0/18;
set_real_ip_from 190.93.240.0/20;
set_real_ip_from 188.114.96.0/20;
set_real_ip_from 197.234.240.0/22;
set_real_ip_from 198.41.128.0/17;
set_real_ip_from 162.158.0.0/15;
set_real_ip_from 104.16.0.0/13;
set_real_ip_from 104.24.0.0/14;
set_real_ip_from 172.64.0.0/13;
set_real_ip_from 131.0.72.0/22;
set_real_ip_from 2400:cb00::/32;
set_real_ip_from 2606:4700::/32;
set_real_ip_from 2803:f800::/32;
set_real_ip_from 2405:b500::/32;
set_real_ip_from 2405:8100::/32;
set_real_ip_from 2a06:98c0::/29;
set_real_ip_from 2c0f:f248::/32;

real_ip_header CF-Connecting-IP;
```

```sh
nginx -t && systemctl reload nginx
```

**6. Kunci origin agar hanya Cloudflare yang bisa masuk**

Karena IP origin masih bisa bocor (log lama, DNS history), batasi port 80/443 hanya dari
rentang IP Cloudflare. **Simpan satu rule SSH lebih dulu agar tidak terkunci dari server.**

```sh
ufw delete allow 'Nginx Full'
ufw delete allow 80/tcp
ufw delete allow 443/tcp

for cidr in 173.245.48.0/20 103.21.244.0/22 103.22.200.0/22 103.31.4.0/22 \
            141.101.64.0/18 108.162.192.0/18 190.93.240.0/20 188.114.96.0/20 \
            197.234.240.0/22 198.41.128.0/17 162.158.0.0/15 104.16.0.0/13 \
            104.24.0.0/14 172.64.0.0/13 131.0.72.0/22; do
  ufw allow from "$cidr" to any port 80,443 proto tcp
done

ufw status numbered    # pastikan OpenSSH masih ALLOW
```

Setelah ini, `curl https://okx.maulmshr.my.id` **dari server lain** (bukan lewat Cloudflare)
akan ditolak — itu memang tujuannya. Uji dari server sendiri dengan `--resolve` ke 127.0.0.1
masih berfungsi untuk debugging.

#### Catatan Cloudflare lain

- **Port yang di-proxy**: Cloudflare hanya meneruskan port tertentu. Untuk HTTPS: `443, 2053,
  2083, 2087, 2096, 8443`. Kita pakai `443`, jadi aman. Jangan berharap `:8080` bisa diakses
  langsung lewat proxy Cloudflare kecuali memakai port yang didukung.
- **WebSocket** didukung di semua paket, termasuk gratis. Header `Upgrade`/`Connection` di
  nginx sudah cukup.
- **Bot protection**: dashboard ini memakai polling `fetch()` dari JavaScript. Jika
  mengaktifkan **Security → Bots** dengan level agresif atau **Challenge Passage**,
  polling bisa kena tantangan CAPTCHA dan dashboard tampak macet. Tambahkan WAF **Skip**
  rule untuk hostname bot, atau biarkan Bot Fight Mode off.
- **Cache**: konten `/api/state` tidak boleh di-cache. Secara default Cloudflare tidak
  meng-cache HTML/JSON, tetapi bila menambah Cache Rules pastikan path `/api/*` di-set **Bypass**.
- **Timeout**: batas 100 detik untuk koneksi HTTP di paket gratis. Dashboard memakai polling
  pendek, jadi tidak terpengaruh.
- **Certbot + orange cloud**: tantangan HTTP-01 umumnya tetap lolos karena Cloudflare
  meneruskan `/.well-known/acme-challenge/`. Namun bila **Always Use HTTPS** aktif, tantangan
  bisa dialihkan ke HTTPS dan gagal. Cara paling aman: terbitkan cert saat **DNS only**,
  baru kemudian ubah ke **Proxied**.

## 9. Verifikasi Akhir

```sh
# 1. Service aktif
systemctl is-active okx-bot && systemctl is-enabled okx-bot

# 2. Bot menjawab di loopback
curl -s -o /dev/null -w 'direct 8080: %{http_code}\n' http://127.0.0.1:8080/api/state

# 3. Lewat nginx (tanpa bergantung DNS)
curl -s --resolve okx.maulmshr.my.id:443:127.0.0.1 \
     -o /dev/null -w 'https via nginx: %{http_code} (%{time_total}s)\n' \
     https://okx.maulmshr.my.id/

# 4. Redirect HTTP → HTTPS
curl -s -o /dev/null -w '%{http_code} -> %{redirect_url}\n' \
     --resolve okx.maulmshr.my.id:80:127.0.0.1 http://okx.maulmshr.my.id/

# 5. Dari internet (DNS publik)
curl -s -o /dev/null -w 'publik: %{http_code}\n' https://okx.maulmshr.my.id/

# 6. Sertifikat
openssl x509 -in /etc/letsencrypt/live/okx.maulmshr.my.id/fullchain.pem -noout -dates -subject
```

Hasil yang diharapkan di server produksi:

```
direct 8080          : 200
https via nginx      : 200 (0.03s)
http->https redirect : 301 -> https://okx.maulmshr.my.id/
publik               : 200
```

Terakhir, buka `https://okx.maulmshr.my.id` dari browser HP. Dashboard harus tampil,
kartu 5 koin terisi harga live, dan hero menampilkan **Balance (USDT)**.

> **ngrok tidak diperlukan lagi.** Bagian "Akses dari HP (ngrok)" di `README.md` adalah
> cara lama sebelum domain + nginx dipasang.

## 10. Operasional Sehari-hari

```sh
# Log
journalctl -u okx-bot -f                 # ikuti log live
journalctl -u okx-bot --since "1 hour ago"
journalctl -u nginx -f

# Kontrol
systemctl restart okx-bot                # setelah ubah .env / rebuild
systemctl stop okx-bot                   # hentikan (posisi tetap ada di OKX!)
nginx -t && systemctl reload nginx       # setelah ubah config nginx

# Update kode dari git
cd /root/OKX-Trading-Bot
git pull
gofmt -l . && go vet ./... && go build -o okx-bot .
systemctl restart okx-bot
journalctl -u okx-bot -n 30 --no-pager
```

Sebelum `git pull`, hentikan engine dari dashboard agar tidak ada order menggantung saat
restart. Kalau ada posisi terbuka, TP/SL tetap hidup di sisi OKX (`attachAlgoOrds`), tetapi
watchdog bot tidak berjalan selama proses mati — jadi jangan biarkan bot mati lama saat
ada posisi terbuka.

Backup yang penting: `.env` (credential) dan `/etc/nginx/sites-available/`. Simpan di luar
server, jangan pernah commit `.env`.

## 11. Troubleshooting

| Gejala | Penyebab umum | Perbaikan |
| --- | --- | --- |
| `bind: address already in use` | Proses lama masih hidup | `pgrep -af okx-bot`; `systemctl stop okx-bot` atau `kill <PID>`, lalu start lagi |
| Halaman default nginx muncul | `location /` masih `try_files`, atau dua `server_name` sama | Pastikan `proxy_pass http://127.0.0.1:8080;` di site domain |
| `502 Bad Gateway` | Bot mati / belum listen | `systemctl status okx-bot`; `ss -tlnp \| grep 8080` |
| `ERR_TOO_MANY_REDIRECTS` | SSL Cloudflare = Flexible | Ubah ke **Full (strict)** |
| `Error 526` (Cloudflare) | Cert origin invalid/expired | Perbarui Let's Encrypt atau Origin CA cert |
| `Error 522/523` (Cloudflare) | Origin tak terjangkau / ufw memblokir CF | Cek `ufw status`, pastikan IP Cloudflare di-allow |
| `curl` publik timeout, lokal 200 | Port 80/443 diblokir firewall atau A record salah | `ufw status`; `dig +short <domain>` |
| Dashboard kosong, harga 0 | WS ke OKX gagal | Cek log `[WS CONNECTED]` / `wsError`; pastikan outbound 8443 terbuka |
| `code 60018` di kartu koin | Koin tidak ada di environment demo OKX | Ganti instrumen ke koin yang didukung demo |
| Harga stale di belakang Cloudflare | `/api/state` ter-cache | Set Cache Rule **Bypass** untuk `/api/*` |
| Build gagal `go.mod requires go >= 1.25.4` | Go sistem 1.22 | [Bagian 1](#1-instal-go): `GOTOOLCHAIN=auto` atau pasang Go 1.25 manual |
| Certbot gagal `.well-known` | Orange cloud + Always Use HTTPS | Terbitkan saat DNS only, atau pakai Origin CA cert |

## 12. Checklist Keamanan

- [ ] `chmod 600 .env`; `.env` tidak pernah masuk `git status`
- [ ] API key OKX **tanpa** izin withdrawal; untuk live pakai IP whitelist bila tersedia
- [ ] Mulai dari `OKX_MODE=demo`, uji minimal beberapa hari sebelum mempertimbangkan live
- [ ] Port `8080` **tidak** dibuka di ufw; akses publik hanya `443`
- [ ] `ufw` aktif dengan default deny incoming; aturan SSH benar
- [ ] HTTPS dipaksa (redirect 80 → 443); SSL mode **Full (strict)** bila pakai Cloudflare
- [ ] Dashboard ini **tidak punya autentikasi** — siapa pun yang tahu URL bisa mengubah
      konfigurasi dan menekan `emergency close`. Mitigasi minimal:
      - Jangan sebarkan URL; pertimbangkan hostname acak
      - Aktifkan **Cloudflare Access** (Zero Trust, gratis ≤ 50 user) di depan hostname
        untuk mewajibkan login email/OTP — sangat disarankan
      - Atau batasi `location /api/config` dan `/api/emergency` per IP di nginx
- [ ] `certbot renew --dry-run` sukses, atau Origin CA cert terpasang dengan masa berlaku panjang
- [ ] F2 Daily Loss Limit dipertimbangkan untuk dibatasi sejak awal

---

Dokumen ini merefleksikan setup per **2026-10-28**. Untuk perilaku strategi, perhitungan
margin, TP/SL, dan fitur risiko F1–F5, lihat [`README.md`](README.md).
