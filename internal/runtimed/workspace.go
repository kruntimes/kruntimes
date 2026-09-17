package runtimed

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kruntimes/kruntimes/internal/toolcache"
)

func persistentWorkspacePath(name string) string {
	return filepath.Join(workspacePath, "persistent", name)
}

func (c *Controller) prepareToolCache() (string, error) {
	c.toolCacheOnce.Do(func() {
		workspaceRoot := c.WorkspacePath
		if workspaceRoot == "" {
			workspaceRoot = workspacePath
		}
		c.toolCacheRoot = toolcache.Root(workspaceRoot)
		if err := toolcache.Prepare(c.toolCacheRoot); err != nil {
			c.toolCacheErr = err
			return
		}
		helperPath := c.CacheHelperPath
		if helperPath == "" {
			helperPath, c.toolCacheErr = os.Executable()
			if c.toolCacheErr != nil {
				c.toolCacheErr = fmt.Errorf("locate runtimed executable for cache helper: %w", c.toolCacheErr)
				return
			}
		}
		_, c.toolCacheErr = toolcache.InstallHelper(c.toolCacheRoot, helperPath)
	})
	if c.toolCacheErr != nil {
		return "", c.toolCacheErr
	}
	return c.toolCacheRoot, nil
}
