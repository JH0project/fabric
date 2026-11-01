// fabric-db applies explicit operator migrations and imports node identities.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/JH0project/fabric/control/internal/database"
)

func main() {
	if err := run(); err != nil {
		// Driver errors may contain SQL values. Keep operator output secret-free.
		fmt.Fprintln(os.Stderr, "fabric database operation failed; verify connectivity, privileges, schema and input")
		os.Exit(1)
	}
}

func run() error {
	migrate := flag.Bool("migrate", false, "apply versioned schema migrations and exit")
	input := flag.String("import-nodes", "", "import node identities from a JSON file")
	list := flag.Bool("list-nodes", false, "print registered nodes as JSON")
	flag.Parse()
	if flag.NArg() != 0 || (!*migrate && *input == "" && !*list) {
		return errors.New("select migrate, import-nodes or list-nodes")
	}
	var nodes []database.Node
	if *input != "" {
		file, err := os.Open(*input)
		if err != nil {
			return err
		}
		defer file.Close()
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&nodes); err != nil {
			return err
		}
		if err := database.ValidateNodes(nodes); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := database.Open(ctx, os.Getenv("FABRIC_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer store.Close()
	if *migrate {
		if err := store.Migrate(ctx); err != nil {
			return err
		}
		fmt.Println("schema migrations complete")
	}
	if *input != "" {
		if err := store.ImportNodes(ctx, nodes); err != nil {
			return err
		}
		fmt.Printf("imported %d nodes\n", len(nodes))
	}
	if *list {
		result, err := store.Nodes(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	return nil
}
