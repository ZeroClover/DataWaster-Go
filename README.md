# Speed Test Tool

A high-performance command-line speed testing tool for measuring download and upload speeds using Cloudflare's speed test infrastructure.

## Features

- Download speed testing using Cloudflare's speed test API
- Upload speed testing with configurable data sizes
- Real-time speed monitoring with automatic unit conversion
- Configurable parallel connections for maximum throughput
- Network interface binding support
- Graceful shutdown with comprehensive statistics
- Error logging to file

## Compilation

### Prerequisites

- Go 1.23 or later
- Internet connection for testing

### Build Instructions

```bash
# Clone or download the source code
# Navigate to the project directory

# Install dependencies (go.mod already provided)
go mod tidy

# Build the binary
go build -o speedtest main.go

# Or build with optimizations
go build -ldflags="-s -w" -o speedtest main.go
```

### Cross-compilation Examples

```bash
# For Linux AMD64
GOOS=linux GOARCH=amd64 go build -o speedtest-linux-amd64 main.go

# For Windows AMD64
GOOS=windows GOARCH=amd64 go build -o speedtest-windows-amd64.exe main.go

# For macOS ARM64
GOOS=darwin GOARCH=arm64 go build -o speedtest-macos-arm64 main.go
```

## Usage

### Basic Commands

```bash
# Download speed test
./speedtest -d

# Upload speed test
./speedtest -u

# Download test with specific duration (30 seconds)
./speedtest -d -t 30

# Upload test with 8 parallel connections
./speedtest -u -P 8

# Test with specific data size (5 GiB)
./speedtest -d -s 5.0

# Download test with specific interface
./speedtest -d --interface eth0
```

### Command Line Options

| Option | Description | Default |
|--------|-------------|---------|
| `-h`, `--help` | Show help message | - |
| `-d`, `--down` | Perform download speed test (mutually exclusive with `-u`) | - |
| `-u`, `--up` | Perform upload speed test (mutually exclusive with `-d`) | - |
| `-t`, `--time` | Test duration in seconds (0 for continuous) | 0 |
| `-s`, `--size` | Test size in GiB | 0.1 |
| `-P`, `--parallel` | Number of parallel connections | 4 |
| `-I`, `--interface` | Network interface to use | - |
| `--fwmark` | Firewall mark | 0 |
| `--log` | Error log file path | errout.log |

### Example Usage

```bash
# Basic download test
./speedtest --down

# Upload test for 60 seconds with 8 parallel connections
./speedtest --up --time 60 --parallel 8

# Download test using specific network interface
./speedtest --down --interface eth0

# Continuous download test with custom log file
./speedtest --down --log /var/log/speedtest.log

# Upload test with specific interface
./speedtest --up --interface eth0
```

## Output Format

### Real-time Display

The tool displays real-time statistics that update every second:

```
Speed: 125.34 MiB/s
Elapsed Time: 2m 34s
Data Used: 1.23 GiB

```

### Final Statistics

After test completion, comprehensive statistics are displayed:

```
Test Start Time: 2024-01-15 14:30:00
Test End Time: 2024-01-15 14:32:34
Total Test Duration: 2m 34s
Total Data Used: 1.23 GiB
Average Speed: 98.76 MiB/s
Maximum Speed: 145.67 MiB/s
```

## Unit Conversions

The tool automatically selects appropriate units:

- **Speed**: B/s → KiB/s → MiB/s → GiB/s
- **Data**: B → KiB → MiB → GiB → TiB
- **Time**: Shows only non-zero units (e.g., "2m 34s" not "0h 2m 34s")

## Precautions and Important Notes

### Network Considerations

- **Bandwidth Usage**: The tool can consume significant bandwidth. Monitor your data usage, especially on metered connections.
- **Network Impact**: High parallel connection counts may impact other network activities.
- **Firewall**: Ensure outbound HTTPS connections to `speed.cloudflare.com` are allowed.

### Performance Considerations

- **CPU Usage**: Higher parallel connection counts increase CPU usage.
- **Memory Usage**: Large data sizes and parallel connections consume more memory.
- **Disk I/O**: Error logging may impact performance on systems with slow storage.

### System Requirements

- **Network**: Stable internet connection required
- **Permissions**: Write access to the current directory for log files (unless custom path specified)
- **Interfaces**: Network interface binding requires appropriate system permissions

### Troubleshooting

1. **Connection Issues**: Check firewall settings and DNS resolution for `speed.cloudflare.com`
2. **Interface Binding**: Ensure the specified network interface exists and is active
3. **Permission Errors**: Check write permissions for the log file location
4. **Memory Issues**: Reduce parallel connections or data size for resource-constrained systems

### Error Logging

All errors are logged to the specified log file (default: `errout.log`). Monitor this file for:
- Network connectivity issues
- DNS resolution problems
- HTTP request failures
- System resource constraints

## Signal Handling

The tool gracefully handles termination signals:
- **SIGINT** (Ctrl+C): Graceful shutdown with statistics
- **SIGTERM**: Clean termination with final results

## Technical Details

### API Endpoints

- **Download**: `https://speed.cloudflare.com/__down?bytes={size}`
- **Upload**: `https://speed.cloudflare.com/__up`

### Implementation Notes

- Uses chunked transfers for better memory efficiency
- Implements atomic operations for thread-safe statistics
- Supports context-based cancellation for clean shutdown
- Automatic retry logic for failed requests


## License

This tool is provided as-is for speed testing purposes. Ensure compliance with your network provider's terms of service when conducting tests.