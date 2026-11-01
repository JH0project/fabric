package inventory

import "os"

func BusFromEnv() (*Bus, error) {
	cluster := os.Getenv("FABRIC_INVENTORY_CLUSTER")
	if cluster == "" {
		cluster = "fabric"
	}
	return NewBusWithAuth(os.Getenv("FABRIC_NATS_URLS"), cluster, TLSFiles{CA: os.Getenv("FABRIC_NATS_CA"), Certificate: os.Getenv("FABRIC_NATS_CERT"), Key: os.Getenv("FABRIC_NATS_KEY")}, NATSAuth{Username: os.Getenv("FABRIC_NATS_USERNAME"), Password: os.Getenv("FABRIC_NATS_PASSWORD")})
}
