package bootstrap

import (
	"io"
	"os"

	"github.com/spf13/cobra"
)

func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap <destination>",
		Short: "Copy the running manager binary to a shared volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := os.Executable()
			if err != nil {
				return err
			}
			sf, err := os.Open(src)
			if err != nil {
				return err
			}
			defer func() { _ = sf.Close() }()
			df, err := os.OpenFile(args[0], os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				return err
			}
			if _, err := io.Copy(df, sf); err != nil {
				_ = df.Close()
				return err
			}
			if err := df.Close(); err != nil {
				return err
			}
			return os.Chmod(args[0], 0750)
		},
	}
}
