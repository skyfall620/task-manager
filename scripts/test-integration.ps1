
param(
    [Parameter(Mandatory = $true)]
    [string]$TestCommand
)

$ErrorActionPreference = "Stop"

# Parse the configured Go test command.
$parts = @($TestCommand.Trim() -split '\s+')

if ($parts.Count -lt 3 -or $parts[0] -ne "go" -or $parts[1] -ne "test") {
    throw "Expected command: go test <package> [flags]"
}

$testArgs = @($parts[2..($parts.Count - 1)])

# Request the MySQL password.
$secure = Read-Host "MYSQL_PASSWORD" -AsSecureString
$previousRedisAddr = $env:TASKS_TEST_REDIS_ADDR
$previousAuthDSN = $env:AUTH_TEST_MYSQL_DSN

try {
    $redisPort = & docker compose port redis 6379

    if ($LASTEXITCODE -ne 0 -or -not $redisPort) {
        throw "Cannot determine Redis address; start the Redis service first"
    }

    $env:TASKS_TEST_REDIS_ADDR = ([string]$redisPort).Trim()
    $password = [System.Net.NetworkCredential]::new("", $secure).Password

    $env:TASKS_TEST_MYSQL_DSN = "task_manager:$password@tcp(127.0.0.1:3307)/task_manager_test?parseTime=true"
    $env:AUTH_TEST_MYSQL_DSN = $env:TASKS_TEST_MYSQL_DSN

    $password = $null

    # Track test execution and outcomes.
    $state = @{
        Ran     = $false
        Passed  = $false
        Skipped = $false
    }

    # JSON output lets us detect SKIP and tests that did not run.
    & go test @testArgs -json | ForEach-Object {
        $line = $_

        try {
            $event = $line | ConvertFrom-Json
        }
        catch {
            Write-Host $line
            return
        }

        if ($event.Output) {
            Write-Host -NoNewline $event.Output
        }

        if ($event.Test) {
            switch ($event.Action) {
                "run"  { $state.Ran = $true }
                "pass" { $state.Passed = $true }
                "skip" { $state.Skipped = $true }
            }
        }
    }

    $testExitCode = $LASTEXITCODE

    if ($testExitCode -ne 0) {
        throw "Integration tests failed (exit code $testExitCode)"
    }

    if ($state.Skipped) {
        throw "Integration tests contain SKIP"
    }

    if (-not $state.Ran -or -not $state.Passed) {
        throw "No integration tests completed successfully"
    }

    Write-Host "Integration tests: PASS"
}
finally {
    Remove-Item Env:TASKS_TEST_MYSQL_DSN -ErrorAction SilentlyContinue
    $env:AUTH_TEST_MYSQL_DSN = $previousAuthDSN
    $env:TASKS_TEST_REDIS_ADDR = $previousRedisAddr
    $password = $null

    if ($secure) {
        $secure.Dispose()
    }
}
