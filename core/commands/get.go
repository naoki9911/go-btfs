package commands

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	gopath "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/TRON-US/go-btfs/core/commands/cmdenv"
	"github.com/TRON-US/go-btfs/core/commands/e"

	cmds "github.com/TRON-US/go-btfs-cmds"
	files "github.com/TRON-US/go-btfs-files"
	"github.com/TRON-US/interface-go-btfs-core/options"
	"github.com/TRON-US/interface-go-btfs-core/path"
	"github.com/ipfs/go-cid"
	"github.com/whyrusleeping/tar-utils"
	"gopkg.in/cheggaaa/pb.v1"
)

var ErrInvalidCompressionLevel = errors.New("compression level must be between 1 and 9")

const (
	outputOptionName           = "output"
	archiveOptionName          = "archive"
	compressOptionName         = "compress"
	compressionLevelOptionName = "compression-level"
	getMetaDisplayOptionName   = "meta"
	decryptName                = "decrypt"
	privateKeyName             = "private-key"
	repairShardsName           = "repair-shards"
)

var GetCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Download BTFS objects.",
		ShortDescription: `
Stores to disk the data contained an BTFS or BTNS object(s) at the given path.

By default, the output will be stored at './<btfs-path>', but an alternate
path can be specified with '--output=<path>' or '-o=<path>'.

To output a TAR archive instead of unpacked files, use '--archive' or '-a'.

To compress the output with GZIP compression, use '--compress' or '-C'. You
may also specify the level of compression by specifying '-l=<1-9>'.

To output just the metadata of this BTFS node, use '--meta' or '-m'.

To repair missing shards of a Reed-Solomon encoded file, use '--repair-shards' or '-rs'.
If '--meta' or '-m' is enabled, this option is ignored.
`,
	},

	Arguments: []cmds.Argument{
		cmds.StringArg("btfs-path", true, false, "The path to the BTFS object(s) to be outputted.").EnableStdin(),
	},
	Options: []cmds.Option{
		cmds.StringOption(outputOptionName, "o", "The path where the output should be stored."),
		cmds.BoolOption(archiveOptionName, "a", "Output a TAR archive."),
		cmds.BoolOption(compressOptionName, "C", "Compress the output with GZIP compression."),
		cmds.IntOption(compressionLevelOptionName, "l", "The level of compression (1-9)."),
		cmds.BoolOption(getMetaDisplayOptionName, "m", "Display token metadata"),
		cmds.BoolOption(decryptName, "d", "Decrypt the file."),
		cmds.StringOption(privateKeyName, "pk", "The private key to decrypt file."),
		cmds.StringOption(repairShardsName, "rs", "Repair the list of shards. Multihashes separated by ','."),
		cmds.BoolOption(quietOptionName, "q", "Quiet mode: perform get operation without writing to anywhere. Same as using -o /dev/null."),
	},
	PreRun: func(req *cmds.Request, env cmds.Environment) error {
		_, err := getCompressOptions(req)
		return err
	},
	RunTimeout: 5 * time.Minute,
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		cmplvl, err := getCompressOptions(req)
		if err != nil {
			return err
		}

		api, err := cmdenv.GetApi(env, req)
		if err != nil {
			return err
		}

		p := path.New(req.Arguments[0])

		decrypt, _ := req.Options[decryptName].(bool)
		privateKey, _ := req.Options[privateKeyName].(string)
		meta, _ := req.Options[getMetaDisplayOptionName].(bool)
		var repairs []cid.Cid
		if rs, ok := req.Options[repairShardsName].(string); ok {
			rshards := strings.Split(rs, ",")
			for _, rshard := range rshards {
				rcid, err := cid.Parse(rshard)
				if err != nil {
					return err
				}
				repairs = append(repairs, rcid)
			}
		}

		opts := []options.UnixfsGetOption{
			options.Unixfs.Decrypt(decrypt),
			options.Unixfs.PrivateKey(privateKey),
			options.Unixfs.Metadata(meta),
			options.Unixfs.Repairs(repairs),
		}

		file, err := api.Unixfs().Get(req.Context, p, opts...)
		if err != nil {
			return err
		}

		quiet, _ := req.Options[quietOptionName].(bool)
		if quiet {
			return res.Emit(nil)
		}

		size, err := file.Size()
		if err != nil {
			return err
		}

		res.SetLength(uint64(size))

		archive, _ := req.Options[archiveOptionName].(bool)
		reader, err := fileArchive(file, p.String(), archive, cmplvl)
		if err != nil {
			return err
		}

		return res.Emit(reader)
	},
	PostRun: cmds.PostRunMap{
		cmds.CLI: func(res cmds.Response, re cmds.ResponseEmitter) error {
			req := res.Request()

			v, err := res.Next()
			if err != nil {
				return err
			}

			// quiet mode return
			if v == nil {
				return nil
			}

			outReader, ok := v.(io.Reader)
			if !ok {
				return e.New(e.TypeErr(outReader, v))
			}

			outPath := getOutPath(req)

			cmplvl, err := getCompressOptions(req)
			if err != nil {
				return err
			}

			archive, _ := req.Options[archiveOptionName].(bool)
			gw := getWriter{
				Out:         os.Stdout,
				Err:         os.Stderr,
				Archive:     archive,
				Compression: cmplvl,
				Size:        int64(res.Length()),
			}

			return gw.Write(outReader, outPath)
		},
	},
}

type clearlineReader struct {
	io.Reader
	out io.Writer
}

func (r *clearlineReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	if err == io.EOF {
		// callback
		fmt.Fprintf(r.out, "\033[2K\r") // clear progress bar line on EOF
	}
	return
}

func progressBarForReader(out io.Writer, r io.Reader, l int64) (*pb.ProgressBar, io.Reader) {
	bar := makeProgressBar(out, l)
	barR := bar.NewProxyReader(r)
	return bar, &clearlineReader{barR, out}
}

func makeProgressBar(out io.Writer, l int64) *pb.ProgressBar {
	// setup bar reader
	// TODO: get total length of files
	bar := pb.New64(l).SetUnits(pb.U_BYTES)
	bar.Output = out

	// the progress bar lib doesn't give us a way to get the width of the output,
	// so as a hack we just use a callback to measure the output, then git rid of it
	bar.Callback = func(line string) {
		terminalWidth := len(line)
		bar.Callback = nil
		log.Infof("terminal width: %v\n", terminalWidth)
	}
	return bar
}

func getOutPath(req *cmds.Request) string {
	outPath, _ := req.Options[outputOptionName].(string)
	if outPath == "" {
		trimmed := strings.TrimRight(req.Arguments[0], "/")
		_, outPath = filepath.Split(trimmed)
		outPath = filepath.Clean(outPath)
	}
	return outPath
}

type getWriter struct {
	Out io.Writer // for output to user
	Err io.Writer // for progress bar output

	Archive     bool
	Compression int
	Size        int64
}

func (gw *getWriter) Write(r io.Reader, fpath string) error {
	if gw.Archive || gw.Compression != gzip.NoCompression {
		return gw.writeArchive(r, fpath)
	}
	return gw.writeExtracted(r, fpath)
}

func (gw *getWriter) writeArchive(r io.Reader, fpath string) error {
	// adjust file name if tar
	if gw.Archive {
		if !strings.HasSuffix(fpath, ".tar") && !strings.HasSuffix(fpath, ".tar.gz") {
			fpath += ".tar"
		}
	}

	// adjust file name if gz
	if gw.Compression != gzip.NoCompression {
		if !strings.HasSuffix(fpath, ".gz") {
			fpath += ".gz"
		}
	}

	// create file
	file, err := os.Create(fpath)
	if err != nil {
		return err
	}
	defer file.Close()

	fmt.Fprintf(gw.Out, "Saving archive to %s\n", fpath)
	bar, barR := progressBarForReader(gw.Err, r, gw.Size)
	bar.Start()
	defer bar.Finish()

	_, err = io.Copy(file, barR)
	return err
}

func (gw *getWriter) writeExtracted(r io.Reader, fpath string) error {
	fmt.Fprintf(gw.Out, "Saving file(s) to %s\n", fpath)
	bar := makeProgressBar(gw.Err, gw.Size)
	bar.Start()
	defer bar.Finish()
	defer bar.Set64(gw.Size)

	extractor := &tar.Extractor{Path: fpath, Progress: bar.Add64}
	return extractor.Extract(r)
}

func getCompressOptions(req *cmds.Request) (int, error) {
	cmprs, _ := req.Options[compressOptionName].(bool)
	cmplvl, cmplvlFound := req.Options[compressionLevelOptionName].(int)
	switch {
	case !cmprs:
		return gzip.NoCompression, nil
	case cmprs && !cmplvlFound:
		return gzip.DefaultCompression, nil
	case cmprs && (cmplvl < 1 || cmplvl > 9):
		return gzip.NoCompression, ErrInvalidCompressionLevel
	}
	return cmplvl, nil
}

// DefaultBufSize is the buffer size for gets. for now, 1MB, which is ~4 blocks.
// TODO: does this need to be configurable?
var DefaultBufSize = 1048576

type identityWriteCloser struct {
	w io.Writer
}

func (i *identityWriteCloser) Write(p []byte) (int, error) {
	return i.w.Write(p)
}

func (i *identityWriteCloser) Close() error {
	return nil
}

func fileArchive(f files.Node, name string, archive bool, compression int) (io.Reader, error) {
	cleaned := gopath.Clean(name)
	_, filename := gopath.Split(cleaned)

	// need to connect a writer to a reader
	piper, pipew := io.Pipe()
	checkErrAndClosePipe := func(err error) bool {
		if err != nil {
			_ = pipew.CloseWithError(err)
			return true
		}
		return false
	}

	// use a buffered writer to parallelize task
	bufw := bufio.NewWriterSize(pipew, DefaultBufSize)

	// compression determines whether to use gzip compression.
	maybeGzw, err := newMaybeGzWriter(bufw, compression)
	if checkErrAndClosePipe(err) {
		return nil, err
	}

	closeGzwAndPipe := func() {
		if err := maybeGzw.Close(); checkErrAndClosePipe(err) {
			return
		}
		if err := bufw.Flush(); checkErrAndClosePipe(err) {
			return
		}
		pipew.Close() // everything seems to be ok.
	}

	if !archive && compression != gzip.NoCompression {
		// the case when the node is a file
		r := files.ToFile(f)
		if r == nil {
			return nil, errors.New("file is not regular")
		}

		go func() {
			if _, err := io.Copy(maybeGzw, r); checkErrAndClosePipe(err) {
				return
			}
			closeGzwAndPipe() // everything seems to be ok
		}()
	} else {
		// the case for 1. archive, and 2. not archived and not compressed, in which tar is used anyway as a transport format

		// construct the tar writer
		w, err := files.NewTarWriter(maybeGzw)
		if checkErrAndClosePipe(err) {
			return nil, err
		}

		go func() {
			// write all the nodes recursively
			if err := w.WriteFile(f, filename); checkErrAndClosePipe(err) {
				return
			}
			w.Close()         // close tar writer
			closeGzwAndPipe() // everything seems to be ok
		}()
	}

	return piper, nil
}

func newMaybeGzWriter(w io.Writer, compression int) (io.WriteCloser, error) {
	if compression != gzip.NoCompression {
		return gzip.NewWriterLevel(w, compression)
	}
	return &identityWriteCloser{w}, nil
}
