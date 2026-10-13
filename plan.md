# Tradingbot BTC-EUR — Plan

## 1. Doel

Een langlopende tradingbot in **Go** die BTC handelt op het paar **BTC-EUR** op **Coinbase Advanced Trade**
(fallback naar BTC-USD alleen als BTC-EUR niet beschikbaar is). De bot werkt in twee modi: **papertrading**
en **live trading**, met exact dezelfde strategiecode. De bot is opvraagbaar en bestuurbaar via een
HTTP API, die later door een React-frontend wordt gebruikt.

Uitgangspunten:

- Lange termijn: de bot moet weken/maanden ononderbroken kunnen draaien en na een herstart naadloos verder gaan.
- Geduld boven verlies: de bot verkoopt **nooit met verlies**. Daalt de koers na een aankoop, dan wacht de bot.
- Simpel en voorspelbaar: één positie tegelijk, vaste inleg per aankoop, alleen limit-orders.

---

## 2. Strategie

### 2.0 Coinbase API

De bot gebruikt de **Coinbase Advanced Trade API** voor alles wat met de exchange te maken heeft:

| Doel | Endpoints (indicatief) | Auth | Modus |
|---|---|---|---|
| Koersdata (candles, ticker/best bid-ask) | `market/products/{id}/candles`, `market/products/{id}/ticker` | publiek | paper + live |
| Productregels (min. grootte, increments) | `market/products/{id}` | publiek | paper + live |
| Saldi | `accounts` | API-key | live |
| Orders plaatsen/annuleren/opvragen | `orders`, `orders/batch_cancel`, `orders/historical/...` | API-key | live |
| Fills | `orders/historical/fills` | API-key | live |
| Actuele fee-tarieven (maker/taker) | `transaction_summary` | API-key (alleen `view`) | live + paper (als er een key is) |

- In **paper**-modus gebruikt de bot alleen de publieke marktdata. Orders worden lokaal gesimuleerd, dus
  er is geen API-key nodig. Een optionele key met alleen `view`-recht zorgt ervoor dat paper met je
  echte fee-tarief rekent.
- In **live**-modus verlopen ook saldi, orders en fills via de API (authenticatie via JWT/ES256 met een CDP API-key).
- Beide modi zitten achter dezelfde `Exchange`-interface, zodat de strategiecode niet weet of hij live of paper draait.

### 2.1 Marktdata

- Elke minuut wordt de laatste **1-minuut candle** (OHLCV) van Coinbase opgehaald en opgeslagen.
- De strategie rekent op **10-minuut candles**. Coinbase levert geen 10-minuut granulariteit, dus de bot
  aggregeert 10 × 1-minuut candles tot één 10-minuut candle (open = eerste open, high = max, low = min,
  close = laatste close, volume = som). Het interval is configureerbaar.
- De 1-minuut data wordt ook gebruikt voor het bewaken van de trailing stop en het controleren van orders,
  zodat de bot sneller reageert dan eens per 10 minuten.
- **Warm-up:** bij het opstarten haalt de bot de benodigde historie (50 × 10 min ≈ 8u20m) direct op via de
  candles-API. Hij hoeft dus niet 8 uur te wachten.
- **Backfill:** na downtime worden ontbrekende candles opgehaald, zodat de reeks in de database compleet blijft.
- Alle tijden in UTC.

### 2.2 Trendbepaling (SMA)

Op basis van de **slotkoersen van de laatste 50 candles van 10 minuten**:

```
SMA_kort = gemiddelde van de laatste 10 slotkoersen
SMA_lang = gemiddelde van de laatste 50 slotkoersen
verschil = (SMA_kort − SMA_lang) / SMA_lang

verschil >  +drempel  → STIJGEND
verschil <  −drempel  → DALEND
anders               → VLAK
```

Standaardwaarden (configureerbaar): `sma_short = 10`, `sma_long = 50`, `flat_threshold_pct = 0.5`.
De trend wordt alleen opnieuw berekend bij een **afgesloten** 10-minuut candle.

### 2.3 Kosten

- Fees worden altijd meegerekend, zowel bij live als bij paper.
- **De fee-tarieven worden bij Coinbase opgehaald** (`transaction_summary`): bij het opstarten en daarna
  elke 24 uur. Coinbase-fees hangen af van je volume-tier en kunnen dus veranderen. Het opgehaalde tarief
  wordt gelogd en opgeslagen, en de bot stuurt een melding als het wijzigt.
- Voor de boekhouding van een positie gebruikt de bot altijd de **werkelijk betaalde fee** uit de fills.
  Het opgehaalde tarief is alleen voor het vooraf berekenen van prijsdoelen.
- Bij een fee-wijziging worden de doelen van een open positie herberekend en de rustende order zo nodig
  vervangen (met de controles uit 2.4a). Een winstorder die al gedeeltelijk gevuld is, blijft staan.
- Papertrading gebruikt het opgehaalde tarief als er een API-key (alleen `view`-recht) geconfigureerd is.
  Anders gebruikt hij de terugvalwaarden uit de config (`fees.fallback_maker_pct` / `fallback_taker_pct`).
- Live trading start niet als het fee-tarief niet opgehaald kan worden.
- **Kostprijs per BTC** van een positie = (totaal betaald in EUR + betaalde fees) / gekochte BTC,
  berekend op basis van de werkelijke fills.
- Winstdoelen zijn **netto** (na koop- én verkoopfee). De verkoopprijs voor een netto winst `w` is:

  ```
  P_doel = kostprijs × (1 + w) / (1 − fee_verkoop)
  ```

  Voorbeeld: kostprijs €60.000, w = 6%, maker-fee 0,25% → P_doel = 60.000 × 1,06 / 0,9975 ≈ €63.759.

### 2.4 Orders

- **Alleen limit-orders.**
- **Koop:** limit-order op de huidige koers (optioneel een configureerbare offset eronder,
  `buy_limit_offset_pct`, standaard 0). Wordt de order niet binnen `buy_order_timeout`
  (standaard 10 min) gevuld, dan wordt hij geannuleerd en beslist de bot bij de volgende candle opnieuw.
  Een gedeeltelijke vulling wordt behouden als positie.
- **Winstnemingen (6% en 12%):** worden als **rustende limit-sell-orders** vooraf op de exchange gezet. Ze
  worden dus ook uitgevoerd als de bot even down is, en tellen als maker (lagere fee).
- **Trailing-stop-verkoop:** zodra de stop geraakt wordt, plaatst de bot een limit-sell voor het restant
  op de huidige bid. Is die niet binnen `stop_order_timeout` (standaard 60 s) gevuld, dan annuleert de bot
  hem en plaatst hij opnieuw op de nieuwe bid, net zo lang tot alles verkocht is.
- Elke order krijgt een unieke `client_order_id`, zodat een retry nooit tot een dubbele order leidt.
- Bij het opstarten haalt de bot de productregels op (`base_min_size`, `base_increment`,
  `quote_increment`). Ordergroottes en prijzen worden hierop afgerond (naar beneden voor hoeveelheden).
- **Dust-regel:** blijft na een gedeeltelijke verkoop een restant over dat kleiner is dan het minimum, dan
  wordt bij die stap direct alles verkocht.

### 2.4a Controles vóór elke actie

Voordat de bot een order plaatst, wijzigt of annuleert, doorloopt hij altijd deze checks. Faalt er één,
dan voert hij de actie niet uit:

1. **Toestand:** de actie moet passen bij de huidige toestand van de state machine (bijv. geen +12%-order
   in `IN_POSITIE`, geen kooporder als er al een positie is). De trader is de enige die de toestand
   wijzigt en slaat hem na elke wijziging op, dus de toestand in het geheugen is altijd gelijk aan die
   in de database.
2. **Werkelijk saldo:** de bot vraagt het actuele BTC- en EUR-saldo op bij de exchange (live) of de
   paper-exchange. Daarbij telt hij zowel het vrije saldo als het saldo dat vastzit in openstaande orders.
   - Te verkopen hoeveelheid ≤ min(positie volgens de database, vrij BTC-saldo).
   - Te besteden bedrag ≤ min(`max_budget_eur`, vrij EUR-saldo).
3. **Geen dubbele orders:** de bot haalt eerst de openstaande orders op.
   - Er mag per positie maximaal één actieve verkooporder zijn. Staat er al een verkooporder (op dezelfde of
     een andere prijs) voor deze stap, dan plaatst de bot geen nieuwe.
   - Opnieuw plaatsen (bijv. bij de trailing-stop-verkoop) gebeurt pas nadat bevestigd is dat de vorige
     order geannuleerd is. Wat daarvan al gevuld was, wordt van de hoeveelheid afgetrokken.
   - Er is maximaal één actieve kooporder, en alleen in `ZOEK_INSTAP`.
   - Elke order krijgt een deterministische `client_order_id` (positie-ID + stap + poging). Een retry na
     een netwerkfout levert daardoor nooit een tweede order op.
4. **Afwijking = stoppen met handelen:** wijkt het werkelijke saldo af van wat de database verwacht
   (bijv. BTC handmatig verkocht, of een onbekende order), dan doet de bot niets. Hij logt de afwijking,
   stuurt een melding en gaat in **pauze** tot iemand het via de API hervat.

Alle acties lopen via één goroutine (de trader-loop), zodat er nooit twee acties tegelijk worden uitgevoerd.

### 2.5 Budget

- `max_budget_eur` is de **vaste inleg per aankoop** (bijv. €50). Elke kooporder gebruikt precies dit
  bedrag (inclusief fee), nooit meer. Winst wordt niet herbelegd; het startpunt is altijd hetzelfde.
- Is het beschikbare EUR-saldo lager dan het budget, dan koopt de bot niet en stuurt hij een melding.
- De bot beheert alleen de BTC die **hij zelf** gekocht heeft. Andere BTC op het account worden genegeerd.
- Er is maximaal **één open positie** tegelijk.

### 2.6 Bewuste keuzes

- **Geen stop-loss.** Daalt de koers na aankoop, dan doet de bot niets en wacht hij tot de winstdoelen
  geraakt worden. Dit kan betekenen dat de inleg lange tijd vastzit.
- **Trailing stop wordt door de bot bewaakt**, omdat Coinbase geen trailing stop heeft. Is de bot down
  tijdens fase TRAILING, dan is het restant niet beschermd. Bij herstart wordt de hoogste koers sinds
  activatie uit de candle-historie gereconstrueerd.
- **Afstand van 5%** tussen trailing stop en hoogste koers, zodat normale schommelingen de stop niet direct raken.

---

## 3. State machine

```
WACHT_OP_DATA ──(50 candles beschikbaar)──▶ ZOEK_INSTAP
ZOEK_INSTAP ──(trend STIJGEND + saldo ≥ budget → koop-limit gevuld)──▶ IN_POSITIE
IN_POSITIE ──(sell-limit +6% netto gevuld: 50% verkocht)──▶ DEEL1_VERKOCHT
DEEL1_VERKOCHT ──(sell-limit +12% netto gevuld: 65% van restant verkocht)──▶ TRAILING
TRAILING ──(koers ≤ trailing stop → restant verkocht)──▶ AFGEROND
AFGEROND ──(resultaat vastgelegd + melding)──▶ COOLDOWN
COOLDOWN ──(zie 3.2)──▶ ZOEK_INSTAP
```

### 3.1 Toestanden

| Toestand | Wat de bot doet | Uitgang |
|---|---|---|
| `WACHT_OP_DATA` | Warm-up/backfill tot er 50 afgesloten 10-minuut candles zijn. | → `ZOEK_INSTAP` |
| `ZOEK_INSTAP` | Bij elke afgesloten candle de trend berekenen. Bij STIJGEND een koop-limit plaatsen (zie 2.4). | Koop (deels) gevuld → `IN_POSITIE` |
| `IN_POSITIE` | Rustende sell-limit voor **50%** van de positie op het +6%-doel. Bij daling: niets doen. | Gevuld → `DEEL1_VERKOCHT` |
| `DEEL1_VERKOCHT` | Rustende sell-limit voor **65% van het restant** op het +12%-doel. Bij daling: niets doen. | Gevuld → `TRAILING` |
| `TRAILING` | Hoogste koers bijhouden (slotkoers van elke minuut + de actuele koers). Stop = hoogste koers × (1 − 5%). De stop gaat alleen omhoog. | Koers ≤ stop → restant verkopen → `AFGEROND` |
| `AFGEROND` | Gerealiseerde winst vastleggen, melding sturen, instapprijs onthouden als `vorige_instap`. | → `COOLDOWN` |
| `COOLDOWN` | 5 of 50 **nieuwe** 10-minuut candles afwachten na de exit, afhankelijk van de trend. | Zie 3.2 → `ZOEK_INSTAP` |

Verdeling van de positie: 50% bij +6%, 32,5% bij +12% (65% van de resterende 50%), 17,5% via de trailing stop.
Op het moment dat de trailing stop start (koers ≈ +12%), ligt de stop op ongeveer +6,4%. Ook het laatste
deel wordt dus met winst verkocht.

### 3.2 Herstartlogica (COOLDOWN)

De lengte van de cooldown hangt af van de trend op het moment van de exit (de laatste afgesloten 10-minuut candle):

| Trend bij exit | Betekenis | Cooldown | Daarna |
|---|---|---|---|
| **DALEND** of **VLAK** | De koers is gekeerd | **50 candles** (`cooldown_candles_reversal`) | `ZOEK_INSTAP`: kopen zodra de trend weer STIJGEND is |
| **STIJGEND** | De koers stijgt door | **5 candles** (`cooldown_candles_rising`) | `ZOEK_INSTAP`: is de trend nog STIJGEND, dan direct een nieuwe kooporder |

- Slaat de trend tijdens een korte cooldown om naar DALEND, dan wordt die alsnog een lange cooldown van
  50 candles, gerekend vanaf de exit.
- Na de cooldown gelden altijd de normale regels van `ZOEK_INSTAP`, met dezelfde vaste inleg (`max_budget_eur`).
- De vorige instapprijs (`vorige_instap`) wordt opgeslagen voor de rapportage, maar is geen voorwaarde meer om opnieuw in te stappen.
- Bij de allereerste start is er geen cooldown; de bot gaat direct van `WACHT_OP_DATA` naar `ZOEK_INSTAP`.

---

## 4. Functionele eisen

### 4.1 Looptijd

- `run_duration` (bijv. `"72h"`), waarbij `0` onbepaald betekent.
- Na afloop stopt de bot netjes (graceful shutdown). Een open positie en rustende limit-orders **blijven
  staan**. Bij de volgende start pakt de bot alles weer op (zie 4.3).

### 4.2 Modi

- **Paper:** gebruikt echte Coinbase-marktdata en een gesimuleerde exchange met een eigen saldo
  (`paper_start_eur`). Om het zo realistisch mogelijk te maken:
  - een order die bij plaatsing direct matcht (koop ≥ best ask, verkoop ≤ best bid) wordt meteen gevuld
    tegen de best ask/bid, met de taker-fee;
  - een rustende order wordt gevuld tegen zijn limietprijs, met de maker-fee, zodra een 1-minuut candle
    die ná de plaatsing begint **door** de limiet gaat: candle-low < limiet (koop) of candle-high > limiet
    (verkoop). Alleen raken is niet genoeg, omdat er dan vaak nog andere orders vóór in de rij staan;
  - een order wordt in één keer volledig gevuld (gedeeltelijke vullingen komen alleen live voor);
  - minimumgroottes en afrondingsregels zijn gelijk aan live;
  - de paper-orderboekstatus staat in de database. Na downtime worden de gemiste candles afgespeeld om te
    bepalen welke orders intussen gevuld zouden zijn.
- **Live:** echte orders via de Coinbase Advanced Trade API. Live trading moet expliciet aangezet worden
  (`"mode": "live"`); de standaard is `paper`.

### 4.3 Herstel na herstart

- De volledige strategiestatus staat in de database: toestand, kostprijs, hoeveelheden, `vorige_instap`,
  hoogste koers in TRAILING, actieve order-ID's, het type cooldown (kort/lang) en de cooldown-teller.
- Bij het opstarten:
  1. status laden;
  2. ontbrekende candles backfillen;
  3. open orders en fills sinds de laatste bekende staat opvragen bij de exchange (of paper-simulatie);
  4. gemiste overgangen alsnog doorvoeren (bijv. +6% gevuld tijdens downtime → `DEEL1_VERKOCHT` en de
     +12%-order plaatsen);
  5. in TRAILING de hoogste koers reconstrueren uit de candles sinds activatie.

### 4.4 Risico en kill switch

- Harde limiet: `max_budget_eur` per aankoop (zie 2.5).
- **Pauze:** er worden geen nieuwe aankopen gedaan; een lopende positie wordt nog wel afgehandeld.
- **Stop:** graceful shutdown.
- Pauze en stop zijn te bedienen via de API. De pauzestatus wordt opgeslagen, zodat hij een herstart overleeft.

### 4.5 API

REST/JSON, versie-prefix `/api/v1`.

| Methode | Endpoint | Omschrijving |
|---|---|---|
| GET | `/status` | Modus, toestand, uptime, resterende looptijd, laatste koers, trend, gepauzeerd ja/nee |
| GET | `/position` | Open positie: hoeveelheid, kostprijs, actuele waarde, ongerealiseerde P&L, doelen, trailing stop |
| GET | `/orders` | Orders (filter op status/periode) |
| GET | `/trades` | Uitgevoerde fills, inclusief fees |
| GET | `/positions/history` | Afgeronde posities met gerealiseerde P&L |
| GET | `/candles` | Koershistorie (`interval`, `from`, `to`) |
| GET | `/positions/{id}` | Eén positie met alle stappen en winst (zie 4.7) |
| GET | `/report` | Winstoverzicht per periode (zie 4.7) |
| GET | `/report/export` | Posities en fills als CSV |
| POST | `/control/pause` | Pauzeren |
| POST | `/control/resume` | Hervatten |
| POST | `/control/stop` | Bot stoppen |

- Luistert standaard alleen op `127.0.0.1`.
- Authenticatie met een bearer-token: `Authorization: Bearer <token>`. Staat er geen token in de config
  (`api.token` leeg), dan maakt de bot er zelf een aan en bewaart het in `data/api_token` (naast de
  database, alleen leesbaar voor de eigenaar). Het blijft na een herstart hetzelfde en komt niet in de log.
- CORS is configureerbaar voor de React-frontend (`api.cors_origins`; `"*"` staat alles toe).
- Een CLI-client komt later. Eerst de API.

Details:

- Bedragen staan als tekst in de JSON (`"74787.35"`), zodat er geen precisie verloren gaat. Tijden in
  RFC3339 (UTC).
- Lijsten zitten in een object (`{"orders": [...]}`), zodat er later velden bij kunnen.
- Filters: `from`/`to` als RFC3339 of `JJJJ-MM-DD`; `limit` 1–1000 (standaard 100); `status` bij
  orders: `OPEN`, `FILLED` of `CANCELLED`.
- `/candles`: standaard `interval=10m` over de laatste 24 uur; elk veelvoud van 1 minuut kan (`1m`,
  `1h`, ...), maximaal 10.000 candles per verzoek. Alleen afgesloten candles.
- `/report/export`: `type=fills` (standaard) of `type=positions`; CSV met komma als scheidingsteken.
- `/control/pause` accepteert optioneel `{"reason": "..."}`. Pauzeren en hervatten geven de nieuwe
  status terug; `/control/stop` antwoordt met 202 en stopt de bot daarna.
- Fouten: `{"error": "..."}` met 400 (ongeldige invoer), 401 (token), 404 of 500 (zonder interne details).
- Kan de API niet starten (bijv. poort bezet), dan stopt de bot: zonder API is hij niet te volgen of te bedienen.

### 4.6 Notificaties en logging

- Gestructureerde logging met `log/slog` naar console en bestand (met rotatie).
- Notificaties via een `Notifier`-interface, eerste implementatie **Telegram**. Er wordt een melding
  gestuurd bij:
  - order geplaatst, gevuld of geannuleerd;
  - toestandsovergang;
  - positie afgerond (met P&L);
  - onvoldoende saldo;
  - herhaalde API-fouten (na 3 mislukte rondes op rij, daarna elk uur);
  - start en stop van de bot;
  - een afwijking waardoor de bot stilgezet is, en gewijzigde fees.
- Meldingen gaan altijd naar de log. Is Telegram ingesteld, dan ook naar Telegram:
  - op de achtergrond, zodat een trage of onbereikbare Telegram de bot nooit ophoudt;
  - meldingen binnen 2 seconden worden gebundeld tot één bericht (één ronde = één bericht);
  - bij een 429 wordt de wachttijd van Telegram aangehouden; bij het afsluiten wordt de wachtrij nog verstuurd;
  - het bot-token komt nooit in de log terecht.
- Telegram instellen: maak een bot via [@BotFather](https://t.me/BotFather) (`/newbot`) en zet het
  token in `telegram.bot_token`. Stuur de bot een bericht en open
  `https://api.telegram.org/bot<token>/getUpdates`; het `chat.id` daarin gaat in `telegram.chat_id`.

### 4.7 Rapportage

De bot houdt per positie en in totaal bij of er winst gemaakt wordt en hoeveel. Alle bedragen zijn
**netto, inclusief alle fees**, en worden berekend uit de werkelijke fills.

**Per positie** (open en afgerond):

| Gegeven | Omschrijving |
|---|---|
| Instap | Datum/tijd, gekochte BTC, kostprijs per BTC, totaal geïnvesteerd (incl. fee) |
| Verkopen per stap | Per stap (+6%, +12%, trailing): datum/tijd, verkochte BTC, prijs, opbrengst, fee |
| Gerealiseerd | Opbrengst van de verkochte delen − hun aandeel in de inleg − fees, in EUR en % |
| Ongerealiseerd | Waarde van het restant tegen de actuele bid − het resterende deel van de inleg − de geschatte verkoopfee, in EUR en % |
| Totaal | Gerealiseerd + ongerealiseerd, in EUR en % van de inleg |
| Fees | Totaal betaalde fees voor deze positie |
| Looptijd | Hoe lang de positie open is/was |
| Status | Toestand, actieve order(s), volgend doel of huidige trailing stop |

**Totaaloverzicht** (over alle posities en per periode: dag, week, maand, totaal):

- aantal afgeronde posities, en daarvan met winst / met verlies;
- totale gerealiseerde winst in EUR en %;
- ongerealiseerde winst van de open positie;
- totaal betaalde fees;
- gemiddelde winst en looptijd per positie;
- in paper-modus: actueel paper-saldo t.o.v. `paper_start_eur`.

**Beschikbaar via:**

- API:
  - `GET /api/v1/position` geeft de open positie met de actuele winst;
  - `GET /api/v1/positions/history` geeft de afgeronde posities;
  - `GET /api/v1/positions/{id}` geeft één positie met alle stappen;
  - `GET /api/v1/report?period=day|week|month|all` geeft het totaaloverzicht;
  - `GET /api/v1/report/export?format=csv` geeft alle posities en fills als CSV, bijvoorbeeld voor je administratie.
- Notificatie: een **dagelijks overzicht** (tijdstip configureerbaar via `report.daily_at`) met de
  afgelopen 24 uur, de open positie, het totaal, het paper-vermogen en de toestand van de bot. Was de
  bot op het tijdstip niet actief, dan volgt het overzicht bij de eerstvolgende ronde; bij de
  allereerste start wordt er niet meteen een gestuurd. Bij elke afgeronde positie stuurt de bot een
  melding met het resultaat.
- Log: bij elke fill een regel met de bijgewerkte winst van de positie.

---

## 5. Niet-functionele eisen

### 5.1 Architectuur (Clean Architecture)

```
cmd/tradingbot/main.go            # wiring, config laden, start/stop
internal/
├── domain/                       # pure logica, geen I/O
│   ├── candle.go, order.go, position.go, money.go
│   ├── indicator/                # SMA, trendbepaling
│   └── strategy/                 # state machine, prijsdoelen, fee-berekeningen
├── usecase/                      # applicatielogica + interfaces (ports)
│   ├── ports.go                  # Exchange, MarketData, Repositories, Notifier, Clock
│   ├── marketdata.go             # ophalen, aggregeren, backfill
│   ├── trader.go                 # strategie aansturen, orders beheren
│   ├── recovery.go               # herstel na herstart
│   └── reporting.go              # P&L, status
├── adapter/
│   ├── exchange/coinbase/        # Coinbase Advanced Trade REST client (JWT/ES256)
│   ├── exchange/paper/           # gesimuleerde exchange
│   ├── storage/sqlite/           # repositories (later storage/postgres/)
│   ├── httpapi/                  # REST handlers, auth, CORS
│   └── notify/telegram/
├── config/                       # config.json laden + valideren
└── platform/                     # logging, klok, scheduler
migrations/                       # SQL-migraties
```

- `domain` is volledig deterministisch en heeft geen afhankelijkheden naar buiten.
- `usecase` kent alleen interfaces. Adapters implementeren die interfaces.
- Tijd komt altijd via een `Clock`-interface, zodat tijdsafhankelijke logica testbaar is.

### 5.2 Database

- **SQLite** via `modernc.org/sqlite` (pure Go, geen CGO, dus makkelijk te bouwen op Windows).
- Migraties met `golang-migrate`, per driver in een eigen map (`migrations/sqlite/`). De SQL blijft zo
  dicht mogelijk bij standaard-SQL; alleen driver-specifieke zaken (zoals auto-increment) verschillen.
  De overstap naar **PostgreSQL** vraagt dus een nieuwe adapter plus `migrations/postgres/`.
- Tabellen: `candles_1m`, `orders`, `fills`, `positions`, `events` (audit log) en `kv_store` (toestand
  van de trader en de paper-exchange als JSON).
- Geldbedragen en hoeveelheden als `TEXT` (SQLite) / `NUMERIC` (PostgreSQL).
- In de code geen `float64` voor geld; gebruik `shopspring/decimal`.

### 5.3 Configuratie en secrets

Eén bestand `config.json`, dat niet in git komt. Een `config.example.json` gaat wel in git.
Omgevingsvariabelen mogen waarden overschrijven, maar de bot moet volledig werken met alleen `config.json`
(ook op Windows).

```json
{
  "mode": "paper",
  "product_id": "BTC-EUR",
  "fallback_product_id": "BTC-USD",
  "run_duration": "0",
  "candle_interval": "10m",
  "sma_short": 10,
  "sma_long": 50,
  "flat_threshold_pct": 0.5,
  "max_budget_eur": 50,
  "paper_start_eur": 100,
  "fees": { "refresh_interval": "24h", "fallback_maker_pct": 0.25, "fallback_taker_pct": 0.50 },
  "take_profit": [
    { "net_profit_pct": 6,  "sell_pct_of_remaining": 50 },
    { "net_profit_pct": 12, "sell_pct_of_remaining": 65 }
  ],
  "trailing_stop_pct": 5,
  "buy_limit_offset_pct": 0,
  "buy_order_timeout": "10m",
  "stop_order_timeout": "60s",
  "cooldown_candles_reversal": 50,
  "cooldown_candles_rising": 5,
  "database": { "driver": "sqlite", "dsn": "data/tradingbot.db" },
  "api": { "listen": "127.0.0.1:8080", "token": "…", "cors_origins": [] },
  "coinbase": { "key_name": "…", "private_key": "…" },
  "telegram": { "bot_token": "…", "chat_id": "…" },
  "report": { "daily_at": "08:00", "timezone": "Europe/Amsterdam" },
  "log": { "level": "info", "file": "logs/tradingbot.log" }
}
```

- Papertrading werkt zonder enige key: de Coinbase-keys zijn alleen verplicht in live-modus, Telegram
  is optioneel en het API-token wordt zo nodig automatisch aangemaakt.
- De Coinbase API-key krijgt alleen de rechten **view** en **trade**, nooit **transfer/withdraw**.
- De config wordt bij het opstarten gevalideerd. Bij ongeldige waarden start de bot niet.

### 5.4 Robuustheid

- Retries met exponential backoff bij netwerkfouten en rate limits (HTTP 429).
- Geen crash bij tijdelijke API-fouten: loggen, melden en bij de volgende tick opnieuw proberen.
- Graceful shutdown op SIGINT/SIGTERM (en Ctrl+C op Windows).

### 5.5 Tests (verplicht)

- **Domain (unit, table-driven):** SMA, trendbepaling (stijgend/dalend/vlak rond de drempel), prijsdoelen
  incl. fees, afronding en dust-regel, alle toestandsovergangen van de state machine, en de
  winstberekening (gerealiseerd/ongerealiseerd per stap, met handmatig nagerekende voorbeelden).
- **Scenario-tests** met vaste prijsreeksen, een nep-klok en de paper-exchange:
  - stijging → +6% → +12% → trailing stop geraakt → cooldown → herinstap;
  - daling direct na aankoop → bot doet niets;
  - trailing stop schuift mee omhoog en nooit omlaag;
  - cooldown: exit bij dalende/vlakke trend (50 candles), bij stijgende trend (5 candles), en een
    stijgende trend die tijdens de korte cooldown omslaat naar dalend;
  - fee-tarief wijzigt tijdens een open positie → doelen herberekend, geen dubbele order;
  - onvoldoende saldo, koop-timeout en gedeeltelijke vulling;
  - geen dubbele verkooporder bij herhaalde ticks, retries of een herstart;
  - een actie die niet past bij de toestand wordt geweigerd;
  - saldo wijkt af van de database → geen actie, melding en pauze.
- **Herstel:** bot stoppen in elke toestand, candles laten doorlopen, herstarten en de juiste eindtoestand controleren.
- **Adapters:** Coinbase-client tegen `httptest`-server met opgenomen responses; SQLite-repositories
  tegen een tijdelijke database; API-handlers via `httptest`.
- Draaien met `go test -race ./...`. Streefdoel: ≥ 90% dekking op `domain`.

---

## 6. Fasering

1. ✅ **Domain:** SMA, trend, state machine, fee- en prijsberekeningen + tests.
2. ✅ **Marktdata + opslag:** Coinbase candles, aggregatie, backfill, SQLite + migraties.
3. ✅ **Paper exchange + trader-usecase**, inclusief herstel na herstart en scenario-tests.
4. ✅ **API:** status, positie, orders, P&L en control-endpoints.
5. ✅ **Notificaties en logging.**
6. ✅ **Live trading** via Coinbase: gebouwd en getest tegen een nagebootste Coinbase-API. Nog niet
   geprobeerd met echte keys; zie de checklist hieronder.
7. **Later:** React-frontend, CLI-client, PostgreSQL-adapter, backtesting op opgeslagen data.

### Checklist voor de eerste keer live

1. **Key aanmaken** in het [CDP-portal](https://portal.cdp.coinbase.com/): kies als algoritme
   **ECDSA** (Ed25519 werkt niet voor Advanced Trade), met alleen de rechten **view** en **trade**,
   nooit **transfer**. Stel bij voorkeur een IP-allowlist in.
2. Zet de key name (`organizations/.../apiKeys/...`) en de private key (PEM, mag op één regel met `\n`)
   in `config.json` onder `coinbase`, of in `TRADINGBOT_COINBASE_KEY_NAME` /
   `TRADINGBOT_COINBASE_PRIVATE_KEY`.
3. **Eerst paper mét key:** laat `mode` op `paper` staan. De bot haalt dan je echte fee-tarieven op en
   logt `fee-tarieven van Coinbase`. Zo is de inlog getest zonder dat er iets gehandeld wordt.
   Een 401 betekent: key of rechten kloppen niet, of de klok van de computer loopt verkeerd (het JWT
   is maar 2 minuten geldig; zet tijdsynchronisatie aan, ook op Windows).
4. **Dan live met een klein budget**, bijv. `"max_budget_eur": 10`. Bij het opstarten logt de bot
   `LIVE TRADING` met je EUR- en BTC-saldo; lukt saldi of fees ophalen niet, dan start hij niet.
5. Volg de eerste cyclus via de API en Telegram, en verhoog daarna pas het budget.

Twee punten waarover de Coinbase-documentatie niet expliciet is, en die bij stap 3 en 4 vanzelf blijken:
de query-string hoort volgens de officiële voorbeelden **niet** in de `uri`-claim van het JWT, en de
fee-tarieven komen als fractie (`"0.0060"` = 0,6%). Een tarief boven 5% wordt als fout geweigerd.

---

## 7. Besluiten

- Cooldown: 50 candles als de koers gekeerd is (DALEND/VLAK), 5 candles als hij doorstijgt.
- Drempel voor "vlak": 0,5% verschil tussen SMA10 en SMA50.
- Notificatiekanaal: Telegram.
- Fees: worden bij Coinbase opgehaald, met de config alleen als terugval voor papertrading.
