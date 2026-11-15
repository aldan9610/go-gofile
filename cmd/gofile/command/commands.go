// Package command implements the gofile CLI subcommands.
package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cheggaaa/pb/v3"
	"github.com/urfave/cli/v2"

	"github.com/aldan9610/go-gofile"
)

// newClient builds a gofile.Client from the global flags.
func newClient(c *cli.Context) *gofile.Client {
	options := []gofile.Option{}
	if key := c.String("api-key"); key != "" {
		options = append(options, gofile.WithToken(key))
	}
	if proxy := c.String("proxy-url"); proxy != "" {
		options = append(options, gofile.WithProxyPrefix(proxy))
	}
	return gofile.New(options...)
}

// parseContentRef accepts a gofile link (https://gofile.io/d/XXXX), a raw
// code (XXXX) or a content UUID, and returns the reference to pass to the
// contents endpoint.
func parseContentRef(ref string) string {
	if i := strings.Index(ref, "gofile.io/d/"); i >= 0 {
		rest := ref[i+len("gofile.io/d/"):]
		if j := strings.IndexAny(rest, "?&#/"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	return ref
}

// humanSize renders a byte count with binary units.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// CmdAccount shows account information.
var CmdAccount = &cli.Command{
	Name:      "account",
	Usage:     "Show account information",
	ArgsUsage: "[ACCOUNT_ID]",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "account-id",
			EnvVars: []string{"GOFILE_ACCOUNT_ID"},
			Usage:   "Account ID (resolved from the API key via /accounts/getid when omitted)",
		},
	},
	Action: func(c *cli.Context) error {
		id := c.Args().First()
		if id == "" {
			id = c.String("account-id")
		}
		account, err := newClient(c).Account(c.Context, id)
		if err != nil {
			return err
		}
		fmt.Printf("ID:         %s\n", account.ID)
		fmt.Printf("Email:      %s\n", account.Email)
		fmt.Printf("Tier:       %s\n", account.Tier)
		if account.RootFolder != "" {
			fmt.Printf("RootFolder: %s\n", account.RootFolder)
		}
		return nil
	},
}

// CmdServers lists available store servers.
var CmdServers = &cli.Command{
	Name:  "servers",
	Usage: "List available store servers",
	Action: func(c *cli.Context) error {
		servers, err := newClient(c).Servers(c.Context)
		if err != nil {
			return err
		}
		for _, s := range servers {
			fmt.Printf("%s (%s)\n", s.Name, s.Zone)
		}
		return nil
	},
}

// CmdInfo shows the metadata of a file or folder.
var CmdInfo = &cli.Command{
	Name:      "info",
	Usage:     "Show the metadata of a file or folder",
	ArgsUsage: "LINK_OR_CODE",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "password", Usage: "Password of password-protected contents"},
	},
	Action: func(c *cli.Context) error {
		if c.NArg() == 0 {
			return fmt.Errorf("expected a link, code or content ID")
		}
		content, err := newClient(c).Content(c.Context, parseContentRef(c.Args().First()), c.String("password"))
		if err != nil {
			return err
		}
		if c.Bool("verbose") {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(content)
		}
		fmt.Printf("Name:     %s\n", content.Name)
		fmt.Printf("Type:     %s\n", content.Type)
		fmt.Printf("ID:       %s\n", content.ID)
		fmt.Printf("Code:     %s\n", content.Code)
		fmt.Printf("Size:     %s (%d bytes)\n", humanSize(content.Size), content.Size)
		if content.MD5 != "" {
			fmt.Printf("MD5:      %s\n", content.MD5)
		}
		if content.Mimetype != "" {
			fmt.Printf("Mime:     %s\n", content.Mimetype)
		}
		fmt.Printf("Public:   %v\n", content.Public)
		if content.IsFolder() {
			fmt.Printf("Children: %d\n", content.ChildrenCount)
		} else {
			fmt.Printf("Link:     %s\n", content.DownloadURL())
		}
		return nil
	},
}

// CmdLs recursively lists the contents of a folder.
var CmdLs = &cli.Command{
	Name:      "ls",
	Usage:     "List the contents of a folder",
	ArgsUsage: "LINK_OR_CODE",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "password", Usage: "Password of password-protected contents"},
	},
	Action: func(c *cli.Context) error {
		if c.NArg() == 0 {
			return fmt.Errorf("expected a folder link, code or ID")
		}
		return listFolder(newClient(c), c.Context, parseContentRef(c.Args().First()), c.String("password"), "")
	},
}

func listFolder(client *gofile.Client, ctx context.Context, idOrCode, password, prefix string) error {
	content, err := client.Content(ctx, idOrCode, password)
	if err != nil {
		return err
	}
	fmt.Printf("%s%s/  (%s, %d items)\n", prefix, content.Name, content.Code, content.ChildrenCount)
	for _, child := range content.Children {
		if child.IsFolder() {
			if err := listFolder(client, ctx, child.ID, password, prefix+"    "); err != nil {
				return err
			}
			continue
		}
		fmt.Printf("%s    %s  (%s, %s, md5 %s)\n", prefix, child.Name, child.Code, humanSize(child.Size), child.MD5)
	}
	return nil
}

// parseUploadArgument splits FILE[:NAME] arguments like pd does.
func parseUploadArgument(arg string) (path, name string) {
	if i := strings.LastIndex(arg, ":"); i > 0 && !strings.HasPrefix(arg, ":") {
		// Do not split Windows drive letters (C:\...).
		if !(len(arg) > 1 && arg[1] == ':') {
			return arg[:i], arg[i+1:]
		}
	}
	return arg, ""
}

// CmdUpload uploads files.
var CmdUpload = &cli.Command{
	Name:      "upload",
	Usage:     "Upload files (a guest account is used when no API key is given)",
	ArgsUsage: "FILE[:NAME]...",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "folder", Usage: "Target folder ID (must be owned by the authenticated account)"},
	},
	Action: func(c *cli.Context) error {
		if c.NArg() == 0 {
			return fmt.Errorf("expected at least 1 file")
		}
		client := newClient(c)
		for i := 0; i < c.NArg(); i++ {
			path, name := parseUploadArgument(c.Args().Get(i))
			if name == "" {
				name = filepath.Base(path)
			}
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("failed to open %s: %w", path, err)
			}
			var src io.Reader = f
			if info, statErr := f.Stat(); statErr == nil {
				bar := pb.Start64(info.Size())
				bar.Set("prefix", name+" ")
				defer bar.Finish()
				src = bar.NewProxyReader(f)
			}
			res, err := client.Upload(c.Context, src, name, c.String("folder"))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("failed to upload %s: %w", path, err)
			}
			fmt.Printf("%s\n", name)
			fmt.Printf("  id:       %s\n", res.ID)
			fmt.Printf("  code:     %s\n", res.Code)
			fmt.Printf("  page:     %s\n", res.DownloadPage)
			if link := res.Content.DownloadURL(); link != "" {
				fmt.Printf("  link:     %s\n", link)
			}
			fmt.Printf("  md5:      %s\n", res.MD5)
			if res.GuestToken != "" && c.String("api-key") == "" {
				fmt.Printf("  guestToken: %s (keep it to manage this upload)\n", res.GuestToken)
			}
		}
		return nil
	},
}

// CmdDownload downloads a file or a whole folder.
var CmdDownload = &cli.Command{
	Name:      "download",
	Usage:     "Download a file or a folder",
	ArgsUsage: "LINK_OR_CODE",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "output",
			Aliases: []string{"o"},
			Value:   ".",
			Usage:   "Output directory",
		},
		&cli.StringFlag{
			Name:  "chunk-size",
			Value: "64MiB",
			Usage: "Fetch the file in chunks of this size (supports KiB/MiB/GiB and KB/MB/GB; 0 streams in one request)",
		},
		&cli.StringFlag{Name: "password", Usage: "Password of password-protected contents"},
		&cli.BoolFlag{Name: "no-verify", Usage: "Skip the MD5 verification"},
	},
	Action: func(c *cli.Context) error {
		if c.NArg() == 0 {
			return fmt.Errorf("expected a link, code or content ID")
		}
		chunk, err := parseSize(c.String("chunk-size"))
		if err != nil {
			return fmt.Errorf("invalid --chunk-size: %w", err)
		}
		client := newClient(c)
		content, err := client.Content(c.Context, parseContentRef(c.Args().First()), c.String("password"))
		if err != nil {
			return err
		}
		if content.IsFile() {
			dest := filepath.Join(c.String("output"), content.Name)
			return downloadContent(client, c, content, dest, chunk)
		}
		root := filepath.Join(c.String("output"), folderName(content))
		return downloadFolder(client, c, content, root, chunk)
	},
}

// folderName picks a directory name for a folder content.
func folderName(content *gofile.Content) string {
	if content.Name != "" {
		return content.Name
	}
	return content.Code
}

// counter accumulates downloaded bytes across files for one progress bar.
type counter struct {
	bar  *pb.ProgressBar
	done int64
}

func (c *counter) progress(written, total int64) {
	c.bar.SetTotal(total)
	c.bar.SetCurrent(c.done + written)
}

// downloadContent downloads a single file with a progress bar.
func downloadContent(client *gofile.Client, c *cli.Context, content *gofile.Content, dest string, chunk int64) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	fmt.Printf("Downloading %s (%s)\n", content.Name, humanSize(content.Size))
	bar := pb.Start64(content.Size)
	bar.Set("prefix", content.Name+" ")
	defer bar.Finish()

	md5 := content.MD5
	if c.Bool("no-verify") {
		md5 = ""
	}
	done := &counter{bar: bar}
	err := client.DownloadToFile(c.Context, content.DownloadURL(), dest, gofile.DownloadOptions{
		ChunkSize: chunk,
		MD5:       md5,
		Progress:  done.progress,
	})
	if err != nil {
		return err
	}
	bar.SetCurrent(content.Size)
	fmt.Printf("Saved to %s (md5 verified: %v)\n", dest, md5 != "")
	return nil
}

// downloadFolder recursively downloads every file of a folder.
func downloadFolder(client *gofile.Client, c *cli.Context, folder *gofile.Content, dir string, chunk int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, child := range folder.Children {
		if child.IsFolder() {
			// Nested folders need their own listing.
			sub, err := client.Content(c.Context, child.ID, c.String("password"))
			if err != nil {
				return err
			}
			if err := downloadFolder(client, c, sub, filepath.Join(dir, folderName(&child)), chunk); err != nil {
				return err
			}
			continue
		}
		dest := filepath.Join(dir, child.Name)
		if err := downloadContent(client, c, &child, dest, chunk); err != nil {
			return err
		}
	}
	return nil
}

// CmdRemove deletes owned contents.
var CmdRemove = &cli.Command{
	Name:      "rm",
	Usage:     "Delete contents owned by the authenticated account",
	ArgsUsage: "CONTENT_ID...",
	Flags: []cli.Flag{
		&cli.BoolFlag{Name: "yes", Usage: "Do not ask for confirmation"},
	},
	Action: func(c *cli.Context) error {
		if c.NArg() == 0 {
			return fmt.Errorf("expected at least 1 content ID")
		}
		if !c.Bool("yes") {
			fmt.Printf("Delete %d content(s)? [y/N] ", c.NArg())
			var answer string
			if _, err := fmt.Scanln(&answer); err != nil || !strings.EqualFold(answer, "y") {
				fmt.Println("Aborted")
				return nil
			}
		}
		ids := make([]string, 0, c.NArg())
		for i := 0; i < c.NArg(); i++ {
			ids = append(ids, c.Args().Get(i))
		}
		return newClient(c).DeleteContents(c.Context, ids)
	},
}

// parseSize parses sizes like "1024", "64MiB", "100MB", "1GB".
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	lower := strings.ToLower(s)
	units := []struct {
		suffix string
		multi  int64
	}{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
		{"kb", 1e3}, {"mb", 1e6}, {"gb", 1e9}, {"tb", 1e12},
		{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
	}
	for _, u := range units {
		if strings.HasSuffix(lower, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(lower, u.suffix))
			var v float64
			if _, err := fmt.Sscanf(num, "%g", &v); err != nil {
				return 0, fmt.Errorf("cannot parse size %q", s)
			}
			return int64(v * float64(u.multi)), nil
		}
	}
	var v int64
	if _, err := fmt.Sscanf(lower, "%d", &v); err != nil {
		return 0, fmt.Errorf("cannot parse size %q", s)
	}
	return v, nil
}
