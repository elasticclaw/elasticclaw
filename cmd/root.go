package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile string
	profile string
	jsonOut bool
	quiet   bool
	yes     bool
)

var rootCmd = &cobra.Command{
	Use:   "elasticclaw",
	Short: "Control plane for provisioning and managing OpenClaw agents",
	Long: `ElasticClaw provisions trusted OpenClaw agents from versioned templates,
runs them on pluggable providers, and binds each one to scoped, short-lived identity.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.elasticclaw/hub.yaml)")
	rootCmd.PersistentFlags().StringVar(&profile, "profile", "", "profile to use")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "output as JSON")
	rootCmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "suppress non-essential output")
	rootCmd.PersistentFlags().BoolVarP(&yes, "yes", "y", false, "answer yes to all prompts")

	viper.BindPFlag("profile", rootCmd.PersistentFlags().Lookup("profile"))
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		viper.AddConfigPath(home + "/.elasticclaw")
		viper.SetConfigType("yaml")
		viper.SetConfigName("hub")
	}

	viper.SetEnvPrefix("ELASTICCLAW")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		if !quiet {
			fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
		}
	}
}
