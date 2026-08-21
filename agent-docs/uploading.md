# Uploading

How to get a file into this service and get a URL back, from scratch, with no
help from whoever runs it.

## 1. Get a token

Sign in with GitHub. Nobody has to approve you; the account you sign in as gets
a namespace of its own with limits attached.

From a terminal, if you have the `asset-service` binary:

```sh
asset-service login --url https://assets.example.com
```

It prints a code and a URL. Open the URL, enter the code, and the token is
saved to your config directory. Or use the same flow over HTTP:

```sh
curl -sS -X POST https://assets.example.com/v1/auth/github/start
# {"device_code":"...","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","interval":5}
```

Show the person the `user_code` and the `verification_uri`, then poll until
they have entered it:

```sh
curl -sS -X POST https://assets.example.com/v1/auth/github/token \
  -H 'Content-Type: application/json' \
  -d '{"device_code":"..."}'
# 202 {"status":"pending"}   -> wait `interval` seconds and ask again
# 201 {"token":"asset_...","namespace":"u-octocat","limits":{...}}
```

Poll no faster than `interval` says, and back off further if a reply says
`slow_down`. There is also a page at `/login` that does all of this in a
browser and shows you the token.

The token is shown once. Keep it outside any working tree -- it is a
credential, and it must never be committed.

## 2. Upload

```sh
asset-service upload diagram.png
```

or directly, which is what to do from a script:

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $ASSET_SERVICE_TOKEN" \
  -H "Content-Type: image/png" \
  --data-binary @diagram.png \
  "https://assets.example.com/v1/assets?namespace=u-octocat&filename=diagram.png"
```

The body is the raw file: no multipart form, no base64. Two parameters matter:
`namespace` (where it goes -- yours, unless you have been given more) and
`filename` (kept, and used for the readable half of the key). Set
`Content-Type` correctly; it is what the file will be served as.

Add `&visibility=private` for something that should only be reachable through
a signed, expiring URL.

You get back a manifest:

```json
{
  "key": "u-octocat/diagram-3f7a91c2b04e.png",
  "url": "https://cdn.example.com/u-octocat/diagram-3f7a91c2b04e.png",
  "renditions_status": "pending",
  "renditions": [{"name": "original", "size": 48213, "url": "..."}]
}
```

`201` means it was stored. `200` means those exact bytes were already there
under that name, which is not an error -- uploading the same file twice is
safe and does nothing the second time.

## 3. Use the URL

`url` is permanent. The key contains a hash of the file's bytes, so that URL
can only ever serve that exact file, and it is cached for a year. Put it in
your markdown, your HTML, or your config and never think about invalidation.

If the file changes, upload the new one: it gets a different URL, and the old
one keeps working for anything that already points at it.

## 4. Use the ladder, for images

An uploaded image gets smaller copies made for it in the background: JPEG at
several widths, never wider than the original -- or PNG, where the image really
uses transparency. Fetch the manifest again a few
seconds later:

```sh
curl -sS -H "Authorization: Bearer $TOKEN" \
  https://assets.example.com/v1/assets/u-octocat/diagram-3f7a91c2b04e.png
```

When `renditions_status` is `ready`, `renditions` holds every size, smallest
first, with the original last. Use the narrowest one wide enough for where the
image is being shown -- that is usually the difference between 90 KB and 4 MB.
In HTML, hand the whole ladder to the browser and let it choose:

```html
<img src="ORIGINAL_URL"
     srcset="W320_URL 320w, W640_URL 640w, W1024_URL 1024w"
     sizes="(max-width: 700px) 100vw, 700px"
     loading="lazy" decoding="async" alt="…">
```

`renditions_status` can also be `none` (this kind of file has no smaller
copies -- an STL, a zip, a PDF) or `failed` (they could not be made; the
original is still fine).

The manifest also carries `width` and `height`: the original's own pixel size.
Put them on the tag. An `<img>` with a width and a height reserves the right
space before the image arrives, so the page does not jump when it does, and
that is most of what a page-speed score is measuring.

## 5. Upload video the same way

Upload the camera's file, not something you shrank first. A video gets H.264
MP4 encodes at a couple of widths and a still frame named `poster`, all in the
same `renditions` list. **Sort them by `content_type`**, not by name -- the
poster is an image and has a width like any other rung:

```html
<video controls preload="none" poster="POSTER_URL" width="1920" height="1080">
  <source src="W960_URL"  type="video/mp4" media="(max-width: 960px)">
  <source src="W1920_URL" type="video/mp4">
</video>
```

`preload="none"` with a poster is the whole trick: the page shows the still and
downloads nothing until somebody presses play.

Transcoding is slower than resizing -- minutes, not seconds -- so
`renditions_status` stays `pending` for a while, and how long depends on what
is doing the work rather than on you. The original is
usable the moment the upload returns.

A self-served account cannot upload video: an account nobody has vouched for
may store images and nothing else. Ask whoever runs the service to raise it,
which is the one command at the end of this page.

## What will stop you

A self-served account is held to limits. They are set where ordinary work never
meets them, so if you meet one, look at what you are doing before asking for
more:

| | |
|---|---|
| `413` | The file is larger than your account may upload. |
| `415` | That content type is not allowed for your account. Self-served accounts upload images. |
| `429` | Too many uploads, or too many bytes today. `Retry-After` says how long. |
| `403` | That namespace is not yours to write to. |
| `401` | The token is wrong, revoked, or expired. Sign in again. |
| `409` | Two different files hashed to the same short name. Rename one and re-upload. |

The exact numbers come back with your token when you sign in, in `limits`.

## If you need more

Ask whoever runs the service. They can raise an account's limits
(`asset-service accounts trust <handle>`) or mint you a key for a shared
namespace (`asset-service keys add docs-ci write:docs`). Both are one command
for them and neither requires redeploying anything.
