# Task Manager

## О проекте

REST API для управления задачами внутри команд: регистрация и JWT-аутентификация, роли участников, задачи, история изменений, комментарии и аналитика.

Обязательные технологии ТЗ: Go, MySQL 8, Redis, Docker и Docker Compose, Git, Swagger/OpenAPI.

## Быстрый запуск

Для запуска на Windows, Linux или macOS нужны только Docker с Linux-контейнерами и Docker Compose v2. Все команды выполняются из корня проекта.

1. Создайте `.env` как копию [.env.example](.env.example) через редактор или файловый менеджер.
2. Заполните `JWT_SECRET`, `MYSQL_ROOT_PASSWORD`, `MYSQL_PASSWORD` собственными независимыми значениями. Остальные параметры можно оставить из шаблона.
3. Запустите приложение и хранилища:

   ```console
   docker compose up -d --build
   ```

   Эквивалент при установленном Task: `task rebuild`.

4. Откройте `http://127.0.0.1:8080/healthz` в браузере или HTTP-клиенте. `GET /healthz` должен вернуть HTTP 200 и `{"status":"ok"}`.

Просмотр состояния, логов и остановка:

```console
docker compose ps -a
docker compose logs --tail 100 app migrate mysql redis
docker compose stop
```

Эквивалент остановки: `task stop`. Для запуска приложения Go, Task и golangci-lint на хосте не нужны.

## Конфигурация

| Переменная | Назначение |
|---|---|
| `JWT_SECRET` | Случайный JWT-секрет: не менее 32 байт без окружающих пробелов. |
| `MYSQL_ROOT_PASSWORD` | Пароль root для первоначальной инициализации MySQL. |
| `MYSQL_PASSWORD` | Пароль пользователя из `MYSQL_USER`, используемого приложением и мигратором. |

Остальные настройки приведены в [.env.example](.env.example). По умолчанию API опубликован на `127.0.0.1:8080`, MySQL — на `127.0.0.1:3307`. Redis получает случайный порт; его адрес на хосте показывает `docker compose port redis 6379`. Порт API меняется через `HTTP_PORT`.

Приложение самостоятельно не загружает `.env`: Compose использует его для подстановок. При локальном запуске Go переменные нужно передать в окружение процесса. Изменение паролей в `.env` не обновляет пользователей уже инициализированной MySQL.

Rate Limiting ограничивает запросы по IP-адресу: по умолчанию 10 запросов/с, burst 20. Настройки: RATE_LIMIT_RPS и RATE_LIMIT_BURST. При превышении лимита возвращается HTTP 429 с заголовком Retry-After. Маршрут /healthz не ограничивается. Счётчики хранятся в памяти приложения.

## Миграции

SQL-файлы находятся в [migrations](migrations). При Docker-запуске мигратор ждёт готовности MySQL, применяет миграции и завершается; приложение запускается после его успешного завершения.

Ручное применение:

```console
docker compose run --rm migrate
```

Эквивалент в текущем Taskfile — `task db:create`.

Повторное применение не пересоздаёт уже созданные таблицы. Данные MySQL сохраняются в volume `mysql_data`, в том числе после обычного `docker compose down` (эквивалент — `task down`).

## Примеры API-запросов

### Регистрация

```http
POST /api/v1/register HTTP/1.1
Host: 127.0.0.1:8080
Content-Type: application/json

{"email":"demo@example.test","password":"DemoOnly-Password-123","name":"Demo User"}
```

Успех: HTTP 201 и объект с `id`, `email`, `name`, `created_at`.

### Вход

```http
POST /api/v1/login HTTP/1.1
Host: 127.0.0.1:8080
Content-Type: application/json

{"email":"demo@example.test","password":"DemoOnly-Password-123"}
```

Успех: HTTP 200, поля `access_token`, `token_type` (`Bearer`), `expires_in` в секундах. Подставьте полученный `access_token` вместо `<JWT>` в следующем запросе.

### Создание команды

```http
POST /api/v1/teams HTTP/1.1
Host: 127.0.0.1:8080
Authorization: Bearer <JWT>
Content-Type: application/json

{"name":"Demo team"}
```

Успех: HTTP 201 и объект с `id`, `name`, `created_by`, `created_at`. Текущий пользователь становится owner команды.

## Документация API

- [Swagger/OpenAPI](docs/openapi.yaml) — описание эндпоинтов, моделей запросов и ответов.
- [Postman-коллекция](docs/task-manager.postman_collection.json) — автоматические проверки регистрации, авторизации, команд, ролей, задач, истории, комментариев, Redis и аналитики и Rate Limiting..

Для запуска тестов импортируйте коллекцию в Postman и выполните её целиком через Collection Runner.

Для запуска коллекции с лимитом по умолчанию задайте задержку между запросами не менее 100 мс. При HTTP 429 учитывайте `Retry-After`; коллекция пока не адаптирована к проверке этого ответа.

## Тестирование

Task и golangci-lint — необязательные инструменты для удобства разработки; Docker-запуск от них не зависит.

Официальные инструкции установки для Windows, Linux и macOS: [Task](https://taskfile.dev/docs/installation), [golangci-lint](https://golangci-lint.run/docs/welcome/install/local/). Для конфигурации проекта требуется golangci-lint **v2** с поддержкой используемой версии Go.

### Проверка кода (`task check`)

Полная проверка включает форматирование, линтер, `go vet`, тесты, сборку и `git diff --check`. Те же команды можно запустить без Task:

```console
gofmt -w .
golangci-lint run ./...
go vet ./...
go test ./...
go build ./...
git diff --check
```

При установленных Go, Git, Task и golangci-lint v2 достаточно выполнить `task check`. Только тесты можно запустить через `go test ./...` или `task test`. Без настроенных переменных подключения интеграционные тесты пропускаются: `task check` **не заменяет** `task test:integration`.

### Подготовка тестовой MySQL

Инструкция рассчитана на стандартного пользователя `task_manager` и MySQL на порту `3307`. База `task_manager_test` отделена от прикладной `task_manager`.

1. Запустите хранилища и откройте MySQL-клиент. На приглашение пароля введите свой `MYSQL_ROOT_PASSWORD`:

   ```console
   docker compose up -d --wait mysql redis
   docker compose exec mysql mysql -u root -p
   ```

2. В MySQL-клиенте создайте базу, выдайте права и выйдите:

   ```sql
   CREATE DATABASE IF NOT EXISTS task_manager_test
       CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
   GRANT ALL PRIVILEGES ON task_manager_test.* TO 'task_manager'@'%';
   exit
   ```

3. Примените миграции **к тестовой БД**, а не к прикладной. Пароль `MYSQL_PASSWORD` вводится при запуске и не записывается непосредственно в историю команд.

   **Windows (PowerShell):**

   ```powershell
   $secure = Read-Host 'MYSQL_PASSWORD' -AsSecureString
   try {
       $password = [System.Net.NetworkCredential]::new('', $secure).Password
       $dsn = "mysql://task_manager:${password}@tcp(mysql:3306)/task_manager_test?multiStatements=true"
       docker compose run --rm migrate -path /migrations -database $dsn up
   } finally {
       $dsn = $null
       $password = $null
       $secure.Dispose()
   }
   ```

   **Linux/macOS (Bash):**

   ```bash
   read -r -s -p 'MYSQL_PASSWORD: ' test_password; printf '\n'
   docker compose run --rm migrate -path /migrations -database "mysql://task_manager:${test_password}@tcp(mysql:3306)/task_manager_test?multiStatements=true" up
   unset test_password
   ```

   Обычный `task db:create` применяет миграции к прикладной БД. При появлении новых миграций отдельно примените их и к `task_manager_test`.

**Удаление тестовой базы**, когда она больше не нужна (вместе со всеми её данными):

```console
docker compose exec mysql mysql -u root -p
```

```sql
DROP DATABASE IF EXISTS task_manager_test;
exit
```

Это не затрагивает прикладную базу `task_manager`. При следующем запуске интеграционных тестов тестовую базу потребуется подготовить заново.

### Интеграционные тесты на Linux/macOS

В Bash после подготовки базы задайте существующие переменные тестов. Пароль запрашивается без вывода на экран:

```bash
read -r -s -p 'MYSQL_PASSWORD: ' test_password
printf '\n'
export TASKS_TEST_MYSQL_DSN="task_manager:${test_password}@tcp(127.0.0.1:3307)/task_manager_test?parseTime=true"
export AUTH_TEST_MYSQL_DSN="$TASKS_TEST_MYSQL_DSN"
export TASKS_TEST_REDIS_ADDR="$(docker compose port redis 6379)"
go test ./... -count=1 -v
unset test_password TASKS_TEST_MYSQL_DSN AUTH_TEST_MYSQL_DSN TASKS_TEST_REDIS_ADDR
```

DSN для Go-тестов передаётся без `mysql://`; для контейнера миграций этот префикс нужен. В macOS при использовании другой оболочки сначала откройте `bash`. При запуске `task test` используются те же переменные, но задача не добавляет `-count=1 -v`.

### Интеграционный скрипт Windows

Текущий [test-integration.ps1](scripts/test-integration.ps1) требует Windows PowerShell (`powershell.exe`). После подготовки базы его можно вызвать напрямую из PowerShell:

```powershell
$testCommand = 'go test ./internal/auth ./internal/teams ./internal/tasks ./internal/stats -run ^(TestRepositoryIntegration|TestRedisListCacheIntegration|TestRepositoryCreateRollsBackOnTimestampReadFailure)$ -count=1 -v'
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/test-integration.ps1 -TestCommand $testCommand
```

Эквивалент через Task — `task test:integration`, который также запускает хранилища. Скрипт запрашивает `MYSQL_PASSWORD`, формирует оба тестовых DSN, определяет порт Redis и считает пропущенные тесты ошибкой. Его переменные действуют в дочернем процессе и не настраивают последующий `task test` в родительской оболочке.

Скрипт использует фиксированные `task_manager`, `127.0.0.1:3307`, `task_manager_test`; создание базы, выдача прав и миграции в него не входят. Для Linux/macOS используйте Bash-сценарий выше.
