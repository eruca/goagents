package archive_test

import (
	"testing"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/archive"
	"github.com/eruca/goagents/contextkit/archive/archivetest"
)

func TestMemoryConformance(t *testing.T) {
	archivetest.Run(t, func(_ *testing.T, max int) contextkit.Archive {
		return archive.New(archive.Config{MaxMessagesPerScope: max})
	})
}
