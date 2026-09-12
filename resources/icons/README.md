# branchDAM Agent Icons

This directory contains icon assets for the branchDAM Agent.

## Required Files

### Windows
- `icon.ico` - Application icon (256x256, 128x128, 48x48, 32x32, 16x16)
- `installer-header.bmp` - NSIS installer header (150x57)
- `installer-sidebar.bmp` - NSIS installer sidebar (164x314)

### macOS
- `icon.icns` - macOS application icon

### Linux/General
- `icon.png` - Application icon (256x256)

## Generating Icons

From the SVG source (`icon.svg`), generate the required formats:

```bash
# Install dependencies
brew install imagemagick   # macOS
# or: apt-get install imagemagick  # Linux

# Generate PNG (256x256)
convert icon.svg -resize 256x256 icon.png

# Generate ICO (Windows)
convert icon.svg -resize 256x256 icon_256.png
convert icon.svg -resize 128x128 icon_128.png
convert icon.svg -resize 48x48 icon_48.png
convert icon.svg -resize 32x32 icon_32.png
convert icon.svg -resize 16x16 icon_16.png
convert icon_256.png icon_128.png icon_48.png icon_32.png icon_16.png icon.ico
rm icon_*.png

# Generate ICNS (macOS)
# Requires iconutil on macOS
mkdir icon.iconset
sips -z 16 16 icon.png --out icon.iconset/icon_16x16.png
sips -z 32 32 icon.png --out icon.iconset/icon_16x16@2x.png
sips -z 32 32 icon.png --out icon.iconset/icon_32x32.png
sips -z 64 64 icon.png --out icon.iconset/icon_32x32@2x.png
sips -z 128 128 icon.png --out icon.iconset/icon_128x128.png
sips -z 256 256 icon.png --out icon.iconset/icon_128x128@2x.png
sips -z 256 256 icon.png --out icon.iconset/icon_256x256.png
sips -z 512 512 icon.png --out icon.iconset/icon_256x256@2x.png
sips -z 512 512 icon.png --out icon.iconset/icon_512x512.png
sips -z 1024 1024 icon.png --out icon.iconset/icon_512x512@2x.png
iconutil -c icns icon.iconset -o icon.icns
rm -rf icon.iconset

# Generate installer bitmaps (NSIS)
convert icon.svg -resize 150x57 -gravity center -background #1a1a2e -extent 150x57 installer-header.bmp
convert icon.svg -resize 164x314 -gravity center -background #1a1a2e -extent 164x314 installer-sidebar.bmp
```

## Design Guidelines

- **Primary color**: #1a1a2e (dark navy)
- **Accent color**: #e94560 (coral red)
- **Text color**: #ffffff (white)
- Icon should be recognizable at 16x16 (system tray) and 256x256 (installer)
