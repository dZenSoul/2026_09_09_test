# Documents service

## Запуск через Docker Compose

Для локального запуска нужен только Docker с Compose. Значения по умолчанию безопасны лишь для разработки. При необходимости скопируйте `.env.example` в `.env` и измените пароль PostgreSQL и `ADMIN_TOKEN`; значения `POSTGRES_PASSWORD` и пароль внутри `POSTGRES_DSN` должны совпадать.

Go-зависимости зафиксированы в `vendor/`, поэтому сборка приложения не требует доступа контейнера к `proxy.golang.org`. После изменения `go.mod` или `go.sum` обновите их командой `go mod vendor`.

```sh
docker compose up --build
```

Compose сначала ждёт готовности PostgreSQL, затем однократно применяет версионированные миграции и запускает приложение только после их успешного завершения. API доступен на `http://localhost:8080`, readiness — на `http://localhost:8080/health/ready`, метрики — на `http://localhost:8080/metrics`.

Остановить сервисы, сохранив базу данных и загруженные файлы:

```sh
docker compose down
```

Посмотреть логи всех сервисов или конкретного этапа:

```sh
docker compose logs -f
docker compose logs migrate
docker compose logs app
```

PostgreSQL по умолчанию не публикуется на хост. Для доступа из локального SQL-клиента используйте явный override:

```sh
docker compose -f compose.yaml -f compose.local.yaml up --build
```

Только для полного сброса локальной среды выполните следующую команду. Она безвозвратно удаляет пользователей, документы, метаданные и загруженные файлы из именованных volumes:

```sh
docker compose down --volumes
```

Повторный запуск миграций безопасен: уже применённые версии учитываются в `schema_migrations` и пропускаются.
