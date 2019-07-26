package command

import (
	"log"

	"github.com/Percona-Lab/percona-dbaas-cli/broker/server"
	"github.com/spf13/cobra"
)

// PxcBrokerCmd represents the pxc broker command
var PxcBrokerCmd = &cobra.Command{
	Use:   "pxc-broker",
	Short: "Start PXC broker",
	Run: func(cmd *cobra.Command, args []string) {
		log.Println("Starting broker")
		server, err := server.NewPXCBroker(cmd.Flag("port").Value.String(), cmd.Flags())
		if err != nil {
			log.Println(err)
			return
		}
		server.Start()
	},
}
var skipS3Storage *bool

func init() {
	PxcBrokerCmd.Flags().String("port", "8081", "Broker API port")
}