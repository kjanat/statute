# Branding

Statute's mark is an angular **S** drawn as a route between two square ports.
The solid tile keeps the route readable at favicon sizes. The lowercase wordmark
uses drawn paths, so it needs no installed or downloaded font.

## Assets

The editable SVG sources and derived browser icons live in
[`assets/brand/`](../assets/brand/). They use the repository's license.

| Asset                          | Use                                                                    |
| ------------------------------ | ---------------------------------------------------------------------- |
| `icon.svg`                     | Primary square mark; avatars, README and API header                    |
| `icon-mono.svg`                | Single-color mark; inline SVG inherits `currentColor`                  |
| `logo.svg`                     | Horizontal wordmark; adapts lettering to light/dark browser preference |
| `favicon.svg`                  | Browser tab icon                                                       |
| `favicon.ico`                  | 16, 32 and 48 px fallback icons                                        |
| `apple-touch-icon.png`         | 180 px touch icon, opaque background                                   |
| `icon-192.png`, `icon-512.png` | App/bookmark icons                                                     |
| `site.webmanifest`             | API reference name, icon paths and browser display mode                |

Use the square icon at 16 px or larger. Keep at least one port's width of clear
space around the artwork; do not stretch it, add shadows, or move its ports.
For a fixed-color monochrome export, replace `currentColor` with the desired ink.
An SVG loaded through an image element does not inherit the surrounding text color.

## Colors

| Color      | Value     | Role                          |
| ---------- | --------- | ----------------------------- |
| Indigo     | `#24358B` | Icon tile and browser theme   |
| White      | `#FFFFFF` | Route                         |
| Periwinkle | `#A9C1FF` | Incoming/outgoing ports       |
| Ink        | `#17203A` | Wordmark on light backgrounds |
| Mist       | `#EDF2FF` | Wordmark on dark backgrounds  |

## Regenerating raster icons

Run from the repository root with ImageMagick 7 installed. SVG files remain the
source; rasterization is a maintainer operation, never a Pages build dependency.

```sh
magick -background none -density 768 assets/brand/favicon.svg -resize 192x192 assets/brand/icon-192.png
magick -background none -density 768 assets/brand/favicon.svg -resize 512x512 assets/brand/icon-512.png
magick -background '#24358b' -density 768 assets/brand/favicon.svg -resize 180x180 -alpha remove assets/brand/apple-touch-icon.png
magick -background none -density 768 assets/brand/favicon.svg -define icon:auto-resize=48,32,16 assets/brand/favicon.ico
```

Pages copies these assets to the site root before uploading its artifact. The
root, subpackage redirect and `/api/` page reference the same favicon set. Asset
changes on `master` trigger a deployment, alongside the release-completion trigger.
The manifest adds bookmark metadata; it does not install a service worker or
change the Go module's vanity redirects.
