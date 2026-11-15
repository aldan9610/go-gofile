# go-gofile

A [Gofile](https://gofile.io) client library and CLI for Go, inspired by
[jkawamoto/go-pixeldrain](https://github.com/jkawamoto/go-pixeldrain).

## Features

- **Library** (package `gofile`) with typed methods for the gofile API:
  server listing, account info, guest accounts, content listing (with
  pagination), uploads (streaming multipart), deletion and downloads.
- **CLI** (`gofile`) with `account`, `servers`, `info`, `ls`, `upload`,
  `download` and `rm` commands, progress bars and automatic MD5 verification.
- **Website-token support**: since 2026 gofile validates a client-computed
  `X-Website-Token` (`sha256(userAgent::language::accountToken::window::salt)`
  with a 4-hour rotating window) and answers `error-notPremium` without it.
  go-gofile computes the token for every request and retries with the
  previous window when a request lands right after a bucket boundary.
- **Chunked downloads with resume**: big files are fetched in `Range` chunks
  (default 64 MiB), appended to the destination and verified against the
  MD5 reported by the API. Interrupted downloads resume from the current file
  size.
- **Proxy support**: every request can be routed through a URL-prefix proxy
  (`--proxy-url https://proxy.example/?`), for hosts where gofile is not
  directly reachable. The proxy must forward `User-Agent`, `X-BL`,
  `X-Website-Token`, `Authorization` and `Range` headers (tested with
  `test.cors.workers.dev`).

## Install

```sh
go install github.com/aldan9610/go-gofile/cmd/gofile@latest
```

Or build from a checkout:

```sh
go build -o gofile ./cmd/gofile
```

## Library usage

```go
client := gofile.New(gofile.WithToken(apiKey))

// Inspect a public link
content, err := client.Content(ctx, "iYbh7w32", "") // code, link or UUID
fmt.Println(content.Name, content.Size)

// Upload (uses a guest account when the client has no token)
res, err := client.Upload(ctx, file, "report.pdf", "")
fmt.Println(res.DownloadPage, res.Content.DownloadURL())

// Download with MD5 verification
err = client.DownloadToFile(ctx, content.DownloadURL(), "out.bin",
    gofile.DownloadOptions{ChunkSize: 64 << 20, MD5: content.MD5})
```

## CLI usage

```sh
export GOFILE_API_KEY=...      # optional; guests are used otherwise
export GOFILE_PROXY_URL=...    # optional URL-prefix proxy

gofile servers                          # list store servers
gofile account                          # account info (ID resolved from the API key)
gofile info https://gofile.io/d/XXXX    # metadata of a link
gofile ls https://gofile.io/d/XXXX      # recursive folder listing
gofile upload big.iso                   # upload; prints page + direct link
gofile upload local.bin:renamed.bin     # upload with a different name
gofile download -o out/ XXXX            # file or folder, MD5 verified
gofile download --chunk-size 0 XXXX     # single streaming request
gofile rm --yes <CONTENT_ID>            # delete owned contents
```

Global flags: `--api-key` (`GOFILE_API_KEY`), `--proxy-url`
(`GOFILE_PROXY_URL`), `--verbose`. Note: like most urfave/cli v2 apps,
command flags must come before positional arguments
(`gofile download -o out/ XXXX`, not `gofile download XXXX -o out/`).

## Notes and limitations

- Downloading a file through the API requires an account that owns the file
  or (for public files) any valid account. Freshly uploaded files may answer
  a redirect for a short propagation period.
- The website-token salt is a constant that gofile may rotate; override it
  with `gofile.WithSalt` if downloads suddenly fail with `error-notPremium`.

## License

MIT — see [LICENSE](LICENSE).
