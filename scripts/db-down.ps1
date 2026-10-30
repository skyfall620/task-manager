$ErrorActionPreference = "Stop"

# Read the migration arguments from Docker Compose.
$configJson = (docker compose config --format json) -join "`n"

if ($LASTEXITCODE -ne 0) {
    throw "Failed to read Docker Compose configuration"
}

$config = $configJson | ConvertFrom-Json
$migrateArgs = @($config.services.migrate.command)

if ($migrateArgs.Count -ne 5 -or $migrateArgs[-1] -ne "up") {
    throw "Unexpected migrate service configuration"
}

# Replace 'up' with 'down -all'.
$migrateArgs[-1] = "down"

docker compose run --rm migrate @migrateArgs -all

exit $LASTEXITCODE
