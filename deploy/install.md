# Installeren als systemd-service

De bot draait als systeemgebruiker `tradingbot` vanuit `/opt/tradingbot`. Standaard staat de API op
`127.0.0.1:8080` en is dus alleen vanaf de machine zelf bereikbaar (gebruik een SSH-tunnel of een
reverse proxy als je hem van buiten wilt benaderen).

## 1. Binary bouwen

```sh
make linux          # -> bin/tradingbot-linux-amd64
```

Op een Windows-machine met MSYS make (git-bash) werkt dit niet: die make geeft de omgevingsvariabelen
niet door aan de recepten, waardoor `go` in `C:\WINDOWS\go-build` probeert te bouwen. Bouw dan direct:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" \
  -o bin/tradingbot-linux-amd64 ./cmd/tradingbot
```

De binary is puur Go (geen CGO), dus bouwen op Windows of macOS voor Linux kan zonder meer.

## 2. Gebruiker en mappen

```sh
sudo useradd --system --home-dir /opt/tradingbot --shell /usr/sbin/nologin tradingbot
sudo mkdir -p /opt/tradingbot/{bin,data,logs}
sudo cp bin/tradingbot-linux-amd64 /opt/tradingbot/bin/
sudo cp config.json /opt/tradingbot/config.json      # of: pas config.example.json aan
sudo cp plan.md /opt/tradingbot/                     # optioneel, waar de unit naar verwijst
sudo chmod 600 /opt/tradingbot/config.json           # bevat in live-modus de Coinbase-key
sudo chown -R tradingbot:tradingbot /opt/tradingbot
```

`data/` en `logs/` worden door de bot zelf gevuld (database, `api_token`, roterend logbestand). De
paden in `config.json` zijn **relatief** (`data/tradingbot.db`, `logs/tradingbot.log`), daarom staat
`WorkingDirectory=/opt/tradingbot` in de unit. Verplaats je de bot, pas dan beide aan.

## 3. Secrets buiten config.json (optioneel)

Liever geen key in `config.json`? Zet de overschrijvingen in `/opt/tradingbot/env`:

```sh
TRADINGBOT_COINBASE_KEY_NAME=organizations/.../apiKeys/...
TRADINGBOT_COINBASE_PRIVATE_KEY=-----BEGIN EC PRIVATE KEY-----\n...
TRADINGBOT_API_TOKEN=...
TRADINGBOT_TELEGRAM_BOT_TOKEN=...
TRADINGBOT_TELEGRAM_CHAT_ID=...
```

```sh
sudo chmod 600 /opt/tradingbot/env
sudo chown tradingbot:tradingbot /opt/tradingbot/env
```

De unit leest dit bestand als het bestaat (`EnvironmentFile=-`), dus zonder dit bestand werkt alles ook.

## 4. Unit installeren

```sh
sudo cp deploy/tradingbot.service /etc/systemd/system/
sudo chmod 644 /etc/systemd/system/tradingbot.service
sudo systemctl daemon-reload
sudo systemctl enable --now tradingbot
```

## 5. Controleren

```sh
systemctl status tradingbot
journalctl -u tradingbot -f          # live meekijken (de bot logt ook naar logs/tradingbot.log)
systemctl show tradingbot -p NRestarts -p ExecMainStatus
```

## Beheer

| Actie | Commando |
|---|---|
| Starten / stoppen / herstarten | `systemctl start\|stop\|restart tradingbot` |
| Stoppen via de API (`/control/stop`) | de unit wordt **inactive** en blijft dat: systemd heeft `Restart=on-failure`, dus een bewuste stop (exitcode 0) wordt niet herstart. Weer starten met `systemctl start tradingbot` |
| Nieuwe binary uitrollen | `systemctl stop tradingbot && cp bin/tradingbot-linux-amd64 /opt/tradingbot/bin/ && systemctl start tradingbot` |
| Herstart-teller wissen na een fout | `systemctl reset-failed tradingbot` |
| Tijdelijke stop zonder de bot te laten herstarten | niet nodig: `systemctl stop` eindigt met exitcode 0 |

Wat de unit verder afdwingt:

- **`Restart=on-failure` + `StartLimitBurst=5` / `StartLimitIntervalSec=300`** (in `[Unit]`; in
  `[Service]` negeert systemd ze sinds v230). Een crash of een mislukte start wordt herstart; na 5
  pogingen binnen 5 minuten geeft systemd het op en staat de unit op `failed`. Dat is het gewenste
  gedrag bij een fout in `config.json`: één keer falen, geen herstart-lus.
- **`After=time-sync.target`**: het JWT voor de Coinbase-API is maar 2 minuten geldig. Loopt de klok
  fout, dan start de bot wel maar faalt elke ronde met een 401. Zorg dat NTP/timesyncd loopt.
- **`KillSignal=SIGTERM` + `TimeoutStopSec=30`**: de bot vangt SIGTERM af, rondt de lopende actie af,
  stuurt de laatste Telegram-meldingen (max 15 s) en sluit de database.
- **Sandbox**: `ProtectSystem=strict` maakt het hele systeem read-only; de bot mag alleen in `data/`
  en `logs/` schrijven (`ReadWritePaths`). `PrivateTmp` is nodig omdat SQLite zijn tijdelijke
  bestanden in `/tmp` zet. `ProtectHome=yes` en `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`
  kunnen aan omdat de bot niets uit `$HOME` leest en alleen Coinbase, Telegram en DNS benadert.
  Tijdzones zitten in de binary (`time/tzdata`), dus `/usr/share/zoneinfo` is niet nodig.

## Verwijderen

```sh
sudo systemctl disable --now tradingbot
sudo rm /etc/systemd/system/tradingbot.service
sudo systemctl daemon-reload && sudo systemctl reset-failed
sudo rm -rf /opt/tradingbot
sudo userdel tradingbot
```
