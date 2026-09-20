# DMG Installer Assets

## background.svg

The branded background image for the macOS DMG installer window (660x400).
The top 40% is a dark header (slate-900) with the branchDAM monogram in
teal and the product name. The bottom 60% is white, serving as the content
area where `create-dmg` places the `.app` icon and Applications alias.

### Customization

Edit `background.svg` (plain SVG, any vector editor) to change the header
color, monogram placement, or product name. The file is committed as a
text asset, not a generated binary.

### Layout assumptions

The icon positions in the Makefile and CI workflow are calibrated for
this background:
- `.app` icon: left-center of the content area
- Applications alias: right-center of the content area
- Window size: 660x400

If you change the background dimensions, update `--window-size`,
`--icon`, and `--app-drop-link` coordinates accordingly.
