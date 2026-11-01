package database

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestImportRejectsAmbiguousIdentitiesBeforeSQL(t *testing.T) {
	n := Node{UUID: uuid.New(), ID: 2, Region: "jp", Zone: "jp-1"}
	for _, nodes := range [][]Node{nil, {n, n}, {{UUID: uuid.Nil, ID: 2, Region: "jp", Zone: "jp-1"}}, {{UUID: n.UUID, ID: 0, Region: "jp", Zone: "jp-1"}}, {{UUID: n.UUID, ID: 2, Region: " ", Zone: "jp-1"}}} {
		if err := (&Store{}).ImportNodes(context.Background(), nodes); err == nil {
			t.Fatal("invalid node identities accepted")
		}
	}
}

func TestRejectUnverifiedDatabaseConnection(t *testing.T) {
	for _, dsn := range []string{"postgresql://[::1]/fabric?sslmode=require", "postgresql://[::1]/fabric?sslmode=verify-full"} {
		if _, err := Open(context.Background(), dsn); err == nil {
			t.Fatal("unverified database configuration accepted")
		}
	}
}
