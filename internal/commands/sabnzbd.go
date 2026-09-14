package commands

import (
	"github.com/onedr0p/exportarr/internal/sabnzbd/collector"
	"github.com/onedr0p/exportarr/internal/sabnzbd/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(sabnzbdCmd)
}

var sabnzbdCmd = &cobra.Command{
	Use:     "sabnzbd",
	Aliases: []string{"sab"},
	Short:   "Prometheus Exporter for Sabnzbd",
	Long:    "Prometheus Exporter for Sabnzbd.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := config.LoadSabnzbdConfig(*conf)
		if err != nil {
			return err
		}
		cs, err := buildSabnzbd(c)
		if err != nil {
			return err
		}
		return serveHTTP(cmd.Context(), conf.ScrapeTimeout, singleTargetHandler(cs...))
	},
}

// buildSabnzbd validates a resolved config and constructs the SABnzbd collector.
func buildSabnzbd(c *config.SabnzbdConfig) ([]prometheus.Collector, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	sab, err := collector.NewSabnzbdCollector(c)
	if err != nil {
		return nil, err
	}
	return []prometheus.Collector{sab}, nil
}
