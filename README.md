# Documents service

Кэширующий HTTP-сервис электронных документов. Он регистрирует пользователей, создаёт и завершает сессии, хранит произвольные JSON-значения и бинарные файлы, выдаёт списки с фильтрацией и проверяет доступ владельца, получателя `grant` и любого аутентифицированного пользователя к публичным документам.

## Архитектура

Запрос проходит через HTTP-транспорт в сервисы аутентификации и документов. Метаданные, пользователи, сессии и права хранятся в PostgreSQL; бинарные данные — в приватном файловом хранилище. Успешные `GET`/`HEAD` кэшируются в ограниченном потокобезопасном LRU-кэше процесса. После загрузки и удаления затронутые записи инвалидируются.

Основные пакеты:

- `cmd/server` — сборка зависимостей, HTTP-сервер и graceful shutdown;
- `cmd/migrate` — применение встроенных версионированных миграций;
- `internal/httptransport` — маршрутизация, форматы ответа, лимиты, кэш и наблюдаемость;
- `internal/auth`, `internal/document` — прикладные сценарии и правила доступа;
- `internal/repository/postgres` — PostgreSQL-репозитории и миграции;
- `internal/blob` — атомарное файловое blob-хранилище;
- `internal/cache` — локальный TTL/LRU-кэш.

## Требования

Для рекомендуемого запуска нужны Docker Engine и Docker Compose. Для запуска без контейнеров нужны Go 1.23.3 и PostgreSQL 18; команды проверки дополнительно используют GNU Make и Staticcheck 2024.1.1. Примеры API рассчитаны на `curl`.

## Быстрый запуск через Docker Compose

Демонстрационные значения Compose подходят только для изолированной локальной разработки. Для постоянного окружения скопируйте `.env.example` в `.env`, задайте новые `POSTGRES_PASSWORD` и `ADMIN_TOKEN` и синхронно обновите пароль в `POSTGRES_DSN`.

```sh
docker compose up --build
```

Compose ждёт готовности PostgreSQL, запускает мигратор и поднимает приложение только после успешных миграций. API доступен на `http://localhost:8080`, liveness — на `/health/live`, readiness — на `/health/ready`, Prometheus-метрики — на `/metrics`.

Go-зависимости зафиксированы в `vendor/`, поэтому сборке образа не нужен доступ к модульному proxy. После изменения `go.mod` или `go.sum` выполните `go mod vendor` и зафиксируйте обновлённый каталог.

```sh
docker compose logs -f app
docker compose logs migrate
docker compose down
```

Обычный `docker compose down` сохраняет базу и файлы в именованных volumes `pgdata` и `blobdata`. PostgreSQL по умолчанию не опубликован на хост. Для подключения локального SQL-клиента:

```sh
docker compose -f compose.yaml -f compose.local.yaml up --build
```

## Локальный запуск без Compose

Создайте базу, экспортируйте минимум `POSTGRES_DSN`, `ADMIN_TOKEN` и каталог хранилища, затем отдельно примените миграции и запустите сервер:

```sh
export POSTGRES_DSN='postgres://documents:change-me@localhost:5432/documents?sslmode=disable'
export ADMIN_TOKEN='replace-with-a-long-random-value'
export FILE_STORAGE_ROOT='./data/blobs'
go run ./cmd/migrate
go run ./cmd/server
```

Миграции встроены в бинарник, учитываются в `schema_migrations` и безопасно пропускаются при повторном запуске. Мигратор использует PostgreSQL advisory lock, поэтому параллельные запуски не применят одну версию дважды. Откат существует для разработки и интеграционных тестов на уровне пакета, но production-команда выполняет только миграции вверх.

## Конфигурация

Полный шаблон находится в `.env.example`. Обязательные значения не имеют безопасного production-default: `POSTGRES_DSN` и `ADMIN_TOKEN`.

| Переменная | Default | Назначение |
|---|---:|---|
| `HTTP_HOST`, `HTTP_PORT` | `0.0.0.0`, `8080` | адрес HTTP-сервера |
| `POSTGRES_DSN` | — | PostgreSQL URL с хостом и именем БД |
| `ADMIN_TOKEN` | — | секрет регистрации пользователей |
| `STORAGE_DRIVER` | `filesystem` | драйвер blob-хранилища; в v1 реализован только `filesystem` |
| `FILE_STORAGE_ROOT` | `./data/blobs` | приватный каталог файлов |
| `SESSION_TTL` | `24h` | срок жизни пользовательской сессии |
| `CACHE_TTL` | `5m` | TTL HTTP-кэша |
| `CACHE_MAX_BYTES`, `CACHE_MAX_ITEMS` | `67108864`, `1000` | границы локального LRU-кэша |
| `MAX_REQUEST_BYTES` | `33554432` | максимальный полный HTTP-запрос |
| `MAX_FILE_BYTES` | `26214400` | максимальный файл |
| `MAX_JSON_BYTES` | `1048576` | максимальная JSON-часть |
| `MAX_GRANT_ITEMS` | `100` | максимальное число элементов `grant` до удаления дублей |
| `MAX_LIST_LIMIT` | `100` | максимальный `limit` списка |
| `HTTP_READ_TIMEOUT` | `10s` | тайм-аут чтения и заголовков |
| `HTTP_WRITE_TIMEOUT` | `30s` | тайм-аут записи ответа |
| `HTTP_IDLE_TIMEOUT` | `60s` | keep-alive тайм-аут |
| `HTTP_PROCESSING_TIMEOUT` | `30s` | предел полной обработки запроса |
| `GRACEFUL_SHUTDOWN_TIMEOUT` | `10s` | ожидание активных запросов при остановке |
| `POSTGRES_MAX_CONNS`, `POSTGRES_MIN_CONNS` | `10`, `1` | границы пула соединений |
| `POSTGRES_MAX_CONN_LIFETIME` | `1h` | максимальный возраст соединения |
| `POSTGRES_MAX_CONN_IDLE_TIME` | `30m` | максимальный простой соединения |
| `POSTGRES_HEALTH_CHECK_PERIOD` | `1m` | период проверки пула |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` или `error` |
| `EXPOSE_CACHE_HEADER` | `false` | включает диагностический `X-Cache: HIT\|MISS` |

Параметры `S3_*` зарезервированы конфигурацией, но S3-драйвер в v1 не реализован. Приложение проверяет конфигурацию до запуска и не выводит значения секретов в ошибках.

## API

Все JSON-ответы используют ровно одно верхнеуровневое поле: `response`, `data` или `error`. Исключение — успешное скачивание файла, возвращающее исходные байты. Прикладной `error.code` стабилен и соответствует HTTP-статусу (`400`, `401`, `403`, `404`, `405`, `500`, `501`, `503`). Для `405` возвращается `Allow`.

Если полная обработка не завершилась за `HTTP_PROCESSING_TIMEOUT` до начала
ответа, API возвращает `503` и
`{"error":{"code":503,"text":"request timed out"}}`. У `HEAD` при этом есть
те же статус, `Content-Type` и `Content-Length`, но нет тела. Тайм-аут уже
начавшегося скачивания прерывает и закрывает поток без добавления JSON к
бинарным данным; такой исход отдельно отражается в логах и метрике
`documents_http_timeouts_total`.

Токены передаются именно так, как зафиксировано контрактом:

- административный токен — поле формы `token` при регистрации;
- сессионный токен — поле `meta.token` при загрузке;
- сессионный токен — query-параметр `token` для списков и чтения;
- сессионный токен — единственное поле тела `token` при удалении документа;
- сессионный токен — часть пути при завершении сессии.

Токены не журналируются. Query-токен сохранён только для совместимости с заданным API; перед production-развёртыванием используйте HTTPS и не допускайте журналирования полного URL на прокси. Ниже используются фиктивные значения.

### 1. Регистрация

Логин содержит не менее восьми латинских букв/цифр. Пароль содержит не менее восьми символов, строчную и прописную латинскую букву, цифру и специальный символ.

```sh
curl -sS -X POST http://localhost:8080/api/register \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'token=local-admin-token-change-me' \
  --data-urlencode 'login=testuser1' \
  --data-urlencode 'pswd=Password1!'
```

### 2. Аутентификация

```sh
curl -sS -X POST http://localhost:8080/api/auth \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'login=testuser1' \
  --data-urlencode 'pswd=Password1!'
```

Для следующих примеров сохраните значение `response.token` локально, не добавляя его в исходный код:

```sh
TOKEN='replace-with-response-token'
```

### 3. Загрузка JSON-документа

`meta` и `json` — отдельные multipart-части. JSON может быть объектом, массивом, строкой, числом, boolean или `null`.

```sh
curl -sS -X POST http://localhost:8080/api/docs \
  -F "meta={\"name\":\"example.json\",\"file\":false,\"public\":false,\"token\":\"$TOKEN\",\"grant\":[]}" \
  -F 'json={"message":"hello"}'
```

### 4. Загрузка файла

```sh
curl -sS -X POST http://localhost:8080/api/docs \
  -F "meta={\"name\":\"photo.jpg\",\"file\":true,\"public\":true,\"token\":\"$TOKEN\",\"mime\":\"image/jpeg\",\"grant\":[]}" \
  -F 'file=@./photo.jpg;type=image/jpeg'
```

Имя документа не может быть путём, содержать `..`, `/`, `\\` или управляющие символы. Пользователи из `grant` должны существовать; дубли удаляются, grant владельцу игнорируется.

### 5. Список и фильтры

```sh
curl -sS "http://localhost:8080/api/docs?token=$TOKEN"
curl -sS "http://localhost:8080/api/docs?token=$TOKEN&login=another1&key=public&value=true&limit=20"
curl -sSI "http://localhost:8080/api/docs?token=$TOKEN"
```

Допустимые `key`: `id`, `name`, `mime`, `file`, `public`, `created`. `value` обязателен вместе с `key`. Результат сортируется по `name` по возрастанию, затем по `created` по убыванию и `id` по возрастанию. Пустая выборка — `200` и `{"data":{"docs":[]}}`.

Без `login` возвращаются собственные документы. С чужим `login` возвращаются доступные документы указанного владельца: публичные или выданные через `grant`.

### 6. Чтение JSON или файла

Возьмите `id` из списка:

```sh
DOC_ID='replace-with-document-id'
curl -sS "http://localhost:8080/api/docs/$DOC_ID?token=$TOKEN"
curl -sS -o downloaded.bin "http://localhost:8080/api/docs/$DOC_ID?token=$TOKEN"
curl -sSI "http://localhost:8080/api/docs/$DOC_ID?token=$TOKEN"
```

`HEAD` выполняет ту же аутентификацию и проверку прав, возвращает статус и заголовки `GET`, но никогда не возвращает тело — в том числе при ошибке.

### 7. Удаление и завершение сессии

Удалять документ может только владелец. Токен удаления принимается только в form-urlencoded теле, не в URL:

```sh
curl -sS -X DELETE "http://localhost:8080/api/docs/$DOC_ID" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode "token=$TOKEN"

curl -sS -X DELETE "http://localhost:8080/api/auth/$TOKEN" \
  -H 'Content-Type: application/x-www-form-urlencoded'
```

После logout токен немедленно недействителен. Повторный logout возвращает `401`; повторное удаление — `404`.

## Доступ и кэш

Все операции с документами, включая публичные, требуют действующей сессии. Читать документ может владелец, пользователь из `grant` или любой аутентифицированный пользователь при `public=true`. Удалять может только владелец.

Кэш локален для одного процесса, ограничен числом записей и байтами и имеет TTL. Кэшируются только успешные `GET`/`HEAD` после аутентификации и проверки доступа. Ошибки не кэшируются; токен не входит в ключ. Одновременные miss одного ключа объединяются. Загрузка и удаление инвалидируют списки и содержимое до успешного ответа. Для диагностики локально можно задать `EXPOSE_CACHE_HEADER=true`.

## Проверки

```sh
make tools       # однократно устанавливает зафиксированный Staticcheck
make check       # format, unit/integration tests, race, vet, staticcheck
make test-container # runtime-образ, UID/GID и multipart-файлы на диске
```

Те же проверки можно выполнить отдельно:

```sh
make fmt-check
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...
```

Репозиторные и сквозные тесты с реальной БД включаются переменной `TEST_POSTGRES_DSN`; каждый тест создаёт отдельную временную схему и удаляет её после завершения:

```sh
export TEST_POSTGRES_DSN='postgres://documents:test-password@localhost:5432/documents_test?sslmode=disable'
go test ./...
go test -race ./...
```

Сквозной тест проходит все семь операций через HTTP, реальные сервисы, PostgreSQL и файловое хранилище; проверяет матрицу доступа, JSON и бинарные данные, фильтры, `HEAD`, кэш, инвалидацию, статусы и конкурентные чтения. CI автоматически поднимает PostgreSQL 18.6 и запускает весь набор.

`make test-container` требует доступный Docker Engine. Он собирает и запускает настоящий `scratch` runtime-образ, загружает файл больше `MAX_JSON_BYTES`, скачивает и сравнивает его побайтно, а также проверяет пользователя `65532:65532`, права временного каталога и отсутствие оставшихся multipart-файлов.

## Полный сброс локальных данных

Следующая команда безвозвратно удаляет всех локальных пользователей, сессии, документы, метаданные и файлы в Compose volumes. Убедитесь, что выполняете её в каталоге именно этого проекта:

```sh
docker compose down --volumes
```

Она намеренно не входит в обычный сценарий остановки.

## Ограничения v1

- один экземпляр приложения;
- только локальный in-memory кэш, без Redis и распределённой инвалидации;
- файловый драйвер хранения; S3-конфигурация зарезервирована, но не реализована;
- нет пагинации списка;
- нет поиска внутри произвольного JSON;
- JSON-часть файлового документа нельзя читать отдельной операцией;
- диагностический cache-header выключен по умолчанию;
- rate limiting для регистрации и входа рекомендуется на внешнем reverse proxy.
