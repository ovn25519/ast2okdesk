# ast2okdesk

Сервис интеграции Asterisk и Okdesk: в реальном времени слушает **Asterisk
Manager Interface (AMI)**, выполняет **screen-pop** для входящих звонков очереди
и после завершения разговора **создаёт запись о звонке в Okdesk** со ссылкой на
MP3-запись разговора.

## Возможности

- **Screen-pop.** По событию `AgentCalled` оператору отправляется информация о
  входящем звонке (`POST /api/v1/telephony/messages`) — карточка клиента
  открывается в Okdesk автоматически. Поддерживаются каналы `SIP` и `PJSIP`;
  внутренний номер оператора берётся из имени peer, поэтому в типовом случае
  дополнительная настройка не нужна. Работает для стратегии `ringall`
  (отдельный screen-pop каждому оператору), есть дедупликация.
- **Журналирование звонка.** По `Hangup` сервис дожидается события `Cdr` и
  создаёт запись о разговоре (`POST /api/v1/phone_calls`) с временем,
  длительностью, направлением и ссылкой на MP3.
- **Автопривязка к заявке (опционально).** По номеру клиента находится
  контакт/компания и открытая заявка, к которой привязывается звонок. По
  умолчанию выключена — привязку выполняет координатор.
- **Надёжность.** Сбой Okdesk API не влияет на приём событий; неудачные записи
  складываются в очередь в SQLite и досылаются после восстановления, переживая
  перезапуск сервиса.
- **HTTPS-раздача записей** через отдельный Caddy с сертификатом по DNS-01 и
  ограничением доступа по IP.

## Архитектура

```
Asterisk 16
   │  AMI (события очереди + Cdr)             /var/calls/*.mp3
   ▼                                                 │
okdesk (Go-сервис)  ── SQLite (retry/корреляция/дедуп)
   │  REST API                                       │
   ▼                                                 ▼
Okdesk  ◀──── issue_id / phone_call ─────────  Caddy (HTTPS, allowlist)
```

- **AMI** — единственный источник данных о звонке. Записи разговоров
  производит штатный механизм Asterisk; сервис только реконструирует имя файла.
- **Caddy** (`okdesk-caddy.service`) раздаёт MP3 встроенным `file_server`.
  Сервис `okdesk.service` лишь генерирует `Caddyfile` и вызывает `caddy reload`.

### Структура репозитория

```
cmd/okdesk/            точка входа, сборка и запуск компонентов
internal/ami/          клиент AMI (подключение, Login, реконнект, парсер)
internal/callflow/     диспетчер событий, screen-pop, дедупликация
internal/journal/      финализация звонка (Hangup → Cdr → phone_calls)
internal/okdesk/       клиент REST API Okdesk (screen-pop, звонки, заявки)
internal/recording/    реконструкция имени файла и file_url
internal/retry/        досылка неудачных записей (backoff, ALERT)
internal/cleanup/      плановая уборка SQLite
internal/monitor/      счётчики и периодический дамп состояния
internal/caddy/        генерация Caddyfile и reload
internal/store/        SQLite: корреляция, дедупликация, retry
internal/config/       загрузка, валидация, маскирование конфигурации
deploy/                systemd-юниты
install.sh             установщик
config.example.toml    пример конфигурации
```

## Предпосылки на стороне Asterisk

Сервис **не изменяет конфигурацию Asterisk** — он только слушает AMI. На АТС
должно быть настроено:

1. **AMI-пользователь на чтение:**
   ```ini
   ; /etc/asterisk/manager.conf
   [ast2okdesk]
   secret = <пароль>
   read = agent,call,cdr
   write =                      ; originate/command не нужны
   permit = 127.0.0.1/255.255.255.255
   ```
2. **Менеджерский CDR** (даёт событие `Cdr`):
   ```ini
   ; /etc/asterisk/cdr_manager.conf
   [general]
   enabled = yes
   ```
3. **События вызова оператора** в настройках мониторируемой очереди:
   ```ini
   ; /etc/asterisk/queues.conf
   [support]
   eventwhencalled = yes
   ```
4. **Запись разговоров.** Asterisk пишет MP3 в `recordings.files_dir`
   (`/var/calls`) по шаблону
   `{Uniqueid}-{YYYY-MM-DD-HH_MM}-{CallerIDNum}-s.mp3`, где минута берётся из
   `Cdr.StartTime` в часовом поясе `asterisk.timezone`. Шаблон привязан к текущей
   схеме именования записей на АТС: при её изменении правьте
   `internal/recording`.

## Установка

Требуется Linux/amd64 и root. Установщик скачивает готовый релиз с GitHub,
проверяет контрольные суммы, создаёт системного пользователя `okdesk`
и включает systemd-юниты.

```bash
curl -fsSL https://raw.githubusercontent.com/ovn25519/ast2okdesk/main/install.sh | sudo bash
# либо конкретная версия:
curl -fsSL https://raw.githubusercontent.com/ovn25519/ast2okdesk/main/install.sh | sudo env OKDESK_VERSION=v0.1.0 bash
```

Установщик:

- кладёт в `/opt/ast2okdesk`: `okdesk`, `caddy` (кастомная сборка с плагином
  reg.ru), `config.example.toml`, `README.md`, каталог `data/`;
- создаёт пользователя/группу `okdesk` (nologin);
- устанавливает юниты `okdesk.service` и `okdesk-caddy.service`;
- при первом запуске копирует `config.example.toml` → `config.toml` (0600) и
  создаёт шаблон `caddy.env` (0600);
- **никогда** не перезаписывает существующие `config.toml`, `caddy.env` и БД
  `okdesk.db`; повторный запуск безопасен (идемпотентен).

### Ручная установка из исходников

```bash
make build            # статический бинарник ./okdesk (CGO off)
sudo install -Dm755 okdesk /opt/ast2okdesk/okdesk
sudo install -Dm644 config.example.toml /opt/ast2okdesk/config.example.toml
sudo install -Dm644 deploy/*.service /etc/systemd/system/
```

## Настройка

Всё в одном файле `/opt/ast2okdesk/config.toml` (права 0600). Полный пример —
`config.example.toml`. Обязательные параметры отмечены ниже.

```toml
[asterisk]
timezone = "Asia/Yekaterinburg"   # обязательный IANA-пояс АТС
queue = "support"                 # обязательное имя очереди
ami_host = "localhost"
ami_port = 5038
ami_user = "ast2okdesk"
ami_password = "<AMI_PASSWORD>"   # обязательно, только в config.toml

[okdesk]
base_url = "https://intellektstroy.okdesk.ru"  # обязательно
api_token = "<OKDESK_API_TOKEN>"               # обязательный ключ «Администратор»
telephony_number = 327           # запасной номер, если имя peer нечисловое
incoming_phone_number = "<...>"  # обязательно, → receiver_phone
search_numbers_count = 10        # 1..10
auto_link_issue = false          # false — заявку привязывает координатор вручную
timezone = "Europe/Moscow"       # пояс аккаунта Okdesk

[recordings]
base_url = "https://calls.example.ru:8443/records/"  # обязательно
files_dir = "/var/calls"

[caddy]
web_port = 8443                  # 80/443 использовать нельзя
dns_provider = "regru"
dns_credentials = "/opt/ast2okdesk/caddy.env"
allowed_ips = ["203.0.113.10", "198.51.100.0/24"]  # хотя бы один IP/CIDR

[retry]
max_attempts = 8
initial_backoff_seconds = 5
max_backoff_seconds = 3600

[retention]
correlation_ttl_hours = 24
cleanup_interval_minutes = 10

# Операторы: переопределения нужны только если имя peer не совпадает с
# внутренним номером сотрудника в Okdesk. В типовом случае настраивать нечего.
# [[employees]]
# sip_peer = "ivan"
# okdesk_telephony_number = 327
```

### Учётные данные DNS (caddy.env)

Файл `/opt/ast2okdesk/caddy.env` (0600) подключается к
`okdesk-caddy.service` как `EnvironmentFile` и содержит данные reg.ru:

```ini
REGRU_USERNAME=<логин>
REGRU_PASSWORD=<пароль>
```

### Применение

```bash
sudo systemctl restart okdesk-caddy   # для DNS-кредов / первого старта
sudo systemctl restart okdesk         # перечитает config.toml и перепишет Caddyfile
sudo systemctl status okdesk okdesk-caddy
```

### Таймзоны

Используются **две** зоны и они независимы:

- `asterisk.timezone` — как трактовать времена из `Cdr` (StartTime/AnswerTime/
  EndTime) и в какой зоне формируется минута в имени файла записи;
- `okdesk.timezone` — в какой зоне отправлять `started_at`/`finished_at` в API.

Сервис сам конвертирует время между зонами; менять шаблон имени файла не нужно.

## Как это работает

### Входящий звонок (screen-pop)

1. `QueueCallerJoin` для нашей очереди → корреляция в SQLite
   (`CallerIDNum`, `Uniqueid`, `Linkedid`).
2. `AgentCalled` → из `DestChannel` извлекается peer оператора (каналы `SIP/` и
   `PJSIP/`). Внутренний номер Okdesk определяется так: явное переопределение в
   `[[employees]]` → **если имя peer состоит из цифр, берётся оно само** (типовой
   случай: оператор указал свой внутренний номер в профиле Okdesk) → иначе
   `okdesk.telephony_number`. Затем дедупликация по паре `Uniqueid`+`peer` и
   отправка screen-pop; ошибка screen-pop только логируется — повторов нет (по
   ТЗ). Если номер определить не удалось, screen-pop пропускается с
   предупреждением в логе.

### Завершение разговора (журналирование)

1. `Hangup` по известному `Uniqueid` → звонок помечается завершённым, сервис
   ждёт `Cdr` (до 10 с).
2. По `Cdr`: `started_at = AnswerTime ?: StartTime`, `finished_at = EndTime`,
   `duration = BillableSeconds ?: Duration`, `direction = 0` (входящий),
   `source_phone = CallerIDNum`, `receiver_phone = incoming_phone_number`,
   `file_url = recordings.base_url + имя файла`.
3. Ищется открытая заявка клиента (контакт → компания) и передаётся `issue_id`.
4. Отправляется `POST /api/v1/phone_calls`. **Журналируются все звонки,
   получившие `Cdr`** — и отвеченные, и брошенные.
5. При сбое запись попадает в очередь retry; корреляция и дедупликация удаляются
   после завершения обработки звонка.

Факт ответа оператора берётся из событий очереди (`AgentConnect` /
`QueueCallerAbandon`), а не из `Cdr.Disposition` — последний возвращает
`ANSWERED` даже для брошенного звонка.

### Автопривязка заявки

По умолчанию **выключена**: запись о звонке уходит в Okdesk без привязки к
заявке, и привязку выполняет координатор вручную. Чтобы сервис подбирал заявку
сам, установите `okdesk.auto_link_issue = true`.

Алгоритм (при `auto_link_issue = true`): `contacts/?phone=<последние N цифр>`
(при нескольких — контакт с наименьшим `id`, берётся его компания) →
`issues/list?contact_ids[]=…&status_codes_not[]=completed`. Если контакта нет —
поиск компании по номеру. Приоритет при нескольких заявках: где звонящий
наблюдатель/инициатор → заявка с ближайшим `deadline_at` → без привязки.

## Раздача записей

- Caddy слушает `https://<домен>:<caddy.web_port>`, `tls { dns regru }`
  (DNS-01; порты 80/443 заняты).
- Каталог `recordings.files_dir` отдаётся по пути из `recordings.base_url`
  (например `/records/`); листинг каталогов отключён.
- Доступ только с адресов `caddy.allowed_ips` (записи содержат персональные
  данные); остальным — `403`.
- Caddyfile генерируется сервисом при старте и перезаписывается — ручные правки
  не сохраняются.

## Мониторинг и логи

Сервис логирует структурированно (`log/slog`, text). Раз в минуту выводится
дамп состояния:

- `api_total` / `api_failed` — вызовы Okdesk API;
- `cdr_received` — сколько событий `Cdr` пришло;
- `retry_depth` — глубина очереди досылки;
- `ami_connected` / `ami_reconnects` / `ami_frames` / `ami_last_event` — состояние AMI;
- `files_dir_ok` — доступность каталога записей (при недоступности — предупреждение).

Секреты (токен Okdesk, пароль AMI, DNS-креды) в логи не попадают — конфигурация
маскируется автоматически.

Просмотр логов:

```bash
journalctl -u okdesk -f
journalctl -u okdesk-caddy -f
```

После `retry.max_attempts` неудачных попыток в лог пишется `ALERT`, а запись
удаляется из очереди (требуется ручной разбор).

## Обслуживание

- **БД:** `/opt/ast2okdesk/okdesk.db` (SQLite, WAL). Хранит корреляцию звонков,
  дедупликацию и очередь retry. Уборка выполняется автоматически каждые
  `retention.cleanup_interval_minutes`; TTL корреляции —
  `retention.correlation_ttl_hours`.
- **Обновление:** снова запустите `install.sh` (при необходимости с
  `OKDESK_VERSION`). `config.toml`, `caddy.env` и `okdesk.db` сохраняются.
- **Бэкап:** достаточно сохранить `config.toml`; БД содержит только временные
  служебные данные.
- **Миграция с v0.1.0:** формат AMI-параметров изменён — вместо секции
  `[asterisk.ami]` используются плоские ключи `ami_host`, `ami_port`, `ami_user`,
  `ami_password` в `[asterisk]`. Старый `config.toml` не загрузится (сервис
  сообщит о неизвестных ключах) — переименуйте ключи. Также появился
  `okdesk.auto_link_issue` (по умолчанию `false`): для прежнего поведения с
  автоматической привязкой заявки добавьте `auto_link_issue = true`.

## Разработка

```bash
make test          # go test ./...
make test-race     # go test -race ./...
make vet           # go vet ./...
make fmt           # gofmt
make shellcheck    # проверка install.sh (нужен shellcheck)
make build         # CGO_ENABLED=0, статический бинарник
```

Зависимости — только `github.com/BurntSushi/toml` и `modernc.org/sqlite`
(pure Go, без CGO). Версия Go — 1.24.

## Отступления от первоначального ТЗ

1. Параметры Asterisk объединены в секцию `[asterisk]`, подключение к AMI —
   плоские ключи `ami_host`, `ami_port`, `ami_user`, `ami_password` внутри неё
   (вместо секций `[route]` и `[ami]`).
2. Добавлен параметр `caddy.allowed_ips` — allowlist доступа к записям.
3. Обрабатываются только **входящие** звонки очереди; исходящие не журналируются.
4. Внутренний номер оператора для screen-pop берётся из имени peer (`SIP`/`PJSIP`)
   напрямую; `okdesk.telephony_number` и `[[employees]]` — необязательные
   переопределения (в ТЗ номер выбирался сопоставлением по таблице).
5. Автопривязка заявки сделана отключаемой (`okdesk.auto_link_issue`, по
   умолчанию выключена); в ТЗ звонок привязывался к заявке всегда.

## Лицензия

Apache License 2.0 — см. [LICENSE](LICENSE).
