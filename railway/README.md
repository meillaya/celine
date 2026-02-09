# Railway Deployment Instructions for Kavita

## Quick Deploy

### Option 1: Deploy from GitHub (Recommended)

1. Fork or create a new repo with these files
2. Connect your GitHub repo to Railway
3. Railway will auto-detect the `railway.json` and `docker-compose.yml`

### Option 2: Deploy from Local Directory

```bash
cd /home/mei/Documents/Kavita/railway
railway login
railway init
railway up
```

## Setup Steps

1. **Create Volumes in Railway Dashboard:**
   - Go to your project dashboard
   - Create volume: `kavita-config` → mount to `/config`
   - Create volume: `kavita-data` → mount to `/data`

2. **Set Environment Variables:**
   - `TZ`: Your timezone (e.g., `America/Toronto`)
   - Railway automatically sets `PORT`

3. **Deploy:**
   - Push to GitHub or run `railway up`

## Storage Limitations (CRITICAL)

Railway volumes have size limits:
- **Free**: 0.5GB
- **Hobby**: 5GB  
- **Pro**: 50GB

### For Libraries Larger Than Your Plan:

**Option A: External Object Storage (Recommended)**
Use S3-compatible storage (AWS S3, Cloudflare R2, Backblaze B2):

1. Set up S3 bucket
2. Mount via s3fs or use a sidecar container
3. Configure Kavita to scan the mount point

**Option B: Multiple Smaller Libraries**
Split your collection across multiple Railway projects

**Option C: Use Filebrowser Sidecar**
Deploy a Filebrowser service to upload files directly to the volume

## Accessing Your Library

Once deployed:
1. Visit your Railway-provided domain
2. Create admin account on first launch
3. Add libraries pointing to `/data`

## Uploading Books

Since Railway volumes don't have a web file browser:

**Method 1: Filebrowser Sidecar (Recommended)**
Add to `docker-compose.yml`:
```yaml
  filebrowser:
    image: filebrowser/filebrowser:latest
    volumes:
      - kavita-data:/srv
    ports:
      - '8080:80'
```

**Method 2: Local Sync + Railway CLI**
```bash
railway connect kavita
# Use rsync or scp to transfer files
```

**Method 3: Direct Upload via Kavita**
Use Kavita's built-in upload feature (Settings → Libraries → Upload)

## Updating Kavita

```bash
# Update image tag in docker-compose.yml or use :latest
railway up
```

## Monitoring

- Check Railway dashboard for resource usage
- Volume usage shown in dashboard
- Health check endpoint: `/api/health`

## Troubleshooting

**Permission Issues:**
- We set `PUID=0` and `PGID=0` (root) for Railway compatibility

**Volume Full:**
- Check dashboard → resize volume
- Or use external storage

**Service Won't Start:**
- Check logs in Railway dashboard
- Verify volume is attached correctly

## Cost Considerations

- Railway charges for volume storage per GB/month
- Bandwidth costs apply for data transfer
- Consider Pro plan for 50GB volume if needed

## Alternative: Self-Hosted

If Railway storage limits are too restrictive, consider:
- VPS (DigitalOcean, Hetzner, etc.)
- Home server
- NAS (Synology, TrueNAS)

These support the standard Docker Compose setup in the parent directory.