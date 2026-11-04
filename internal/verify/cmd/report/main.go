// Command report renders the verification facts that the tests recorded as a Markdown report on
// standard output.
//
//	go run ./internal/verify/cmd/report [-dir .verify]
package main

import (
	"flag"
	"fmt"
	"os"

	"maragu.dev/maragudb/internal/verify"
)

func main() {
	dir := flag.String("dir", ".verify", "directory of the facts, as MARAGUDB_VERIFY_DIR was for the tests")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	facts, err := verify.ReadFacts(dir)
	if err != nil {
		return err
	}
	if len(facts) == 0 {
		return fmt.Errorf("no facts in %s; run `make test` first", dir)
	}
	root, err := verify.FindRoot()
	if err != nil {
		return err
	}
	c, err := verify.LoadCatalog(root)
	if err != nil {
		return err
	}
	fmt.Print(verify.Render(facts, c))
	return nil
}
