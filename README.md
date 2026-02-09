# Kavita Docker Setup (LinuxServer.io)

This is a Docker Compose setup for [Kavita](https://www.kavitareader.com/) - a fast, feature-rich, cross-platform reading server.

## Quick Start

1. **Start the container:**
   ```bash
   docker compose up -d
   ```

2. **Access Kavita:**
   Open http://localhost:5000 in your browser

3. **Initial Setup:**
   - Create an admin account on first launch
   - Add your libraries by pointing them to `/data` (mounted from `./data`)

## Directory Structure

```
Kavita/
├── docker-compose.yml    # Docker Compose configuration
├── .env                  # Environment variables
├── config/               # Kavita configuration files
└── data/                 # Your books/comics/manga (mount your media here)
```

## Configuration

Edit `.env` file to customize:
- `PUID`/`PGID`: Set to your user's UID/GID (`id -u` and `id -g`)
- `TZ`: Your timezone (e.g., `America/New_York`, `Europe/London`)
- `KAVITA_PORT`: Change the port if 5000 is in use

## Adding Your Media

Place your books/comics/manga in the `data/` folder, or mount external directories by editing `docker-compose.yml`:

```yaml
volumes:
  - ./config:/config
  - /path/to/your/books:/data/books
  - /path/to/your/comics:/data/comics
```

## Useful Commands

```bash
# Start Kavita
docker compose up -d

# View logs
docker compose logs -f kavita

# Stop Kavita
docker compose down

# Update Kavita
docker compose pull
docker compose up -d

# Check health
docker compose ps
```

## Documentation

- [Kavita Wiki](https://wiki.kavitareader.com/)
- [LSIO Docker Hub](https://hub.docker.com/r/linuxserver/kavita)
- [Kavita GitHub](https://github.com/Kareadita/Kavita)

## Default Login

On first launch, you'll be prompted to create an admin account.

## Ports

- `5000`: Kavita web interface

## Volumes

- `/config`: Configuration files
- `/data`: Your media files (optional)