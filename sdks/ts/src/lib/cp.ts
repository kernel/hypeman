/**
 * Copy operations for transferring files to/from running instances.
 *
 * This module provides functions for copying files between the local filesystem
 * and running hypeman instances, similar to `docker cp`.
 */

import * as fs from 'fs';
import * as path from 'path';

/**
 * Configuration for copy operations.
 */
export interface CpConfig {
  /** Base URL for the hypeman API */
  baseURL: string;
  /** API key (JWT token) for authentication */
  apiKey: string;
}

/**
 * Options for copying files to an instance.
 */
export interface CpToInstanceOptions {
  /** Instance ID to copy to */
  instanceId: string;
  /** Local source path */
  srcPath: string;
  /** Destination path in guest */
  dstPath: string;
  /** Optional: override file mode/permissions */
  mode?: number;
}

/**
 * Options for copying files from an instance.
 */
export interface CpFromInstanceOptions {
  /** Instance ID to copy from */
  instanceId: string;
  /** Source path in guest */
  srcPath: string;
  /** Local destination path */
  dstPath: string;
  /** Follow symbolic links */
  followLinks?: boolean;
}

interface CpRequest {
  direction: 'to' | 'from';
  guest_path: string;
  is_dir?: boolean;
  mode?: number;
  follow_links?: boolean;
}

interface CpFileHeader {
  type: 'header';
  path: string;
  mode: number;
  is_dir: boolean;
  is_symlink?: boolean;
  link_target?: string;
  size: number;
  mtime: number;
}

interface CpEndMarker {
  type: 'end';
  final: boolean;
}

interface CpResult {
  type: 'result';
  success: boolean;
  error?: string;
  bytes_written?: number;
}

interface CpError {
  type: 'error';
  message: string;
  path?: string;
}

type CpMessage = CpFileHeader | CpEndMarker | CpResult | CpError;

/**
 * Sanitizes a path from the server to prevent path traversal attacks.
 * Ensures the path stays within the destination directory.
 */
function sanitizePath(base: string, relativePath: string): string {
  // Normalize the path to resolve . and .. components
  const normalized = path.normalize(relativePath);

  // Reject absolute paths
  if (path.isAbsolute(normalized)) {
    throw new Error(`Invalid path: absolute paths not allowed: ${relativePath}`);
  }

  // Reject paths that escape the destination
  if (normalized.startsWith('..')) {
    throw new Error(`Invalid path: path escapes destination: ${relativePath}`);
  }

  // Join with base and verify the result is under base
  const result = path.join(base, normalized);
  const absBase = path.resolve(base);
  const absResult = path.resolve(result);

  if (!absResult.startsWith(absBase + path.sep) && absResult !== absBase) {
    throw new Error(`Invalid path: path escapes destination: ${relativePath}`);
  }

  return result;
}

/**
 * Builds the WebSocket URL for the cp endpoint.
 */
function buildWsURL(baseURL: string, instanceId: string): string {
  const url = new URL(baseURL);
  // Append to existing pathname instead of overwriting
  const existingPath = url.pathname.endsWith('/') ? url.pathname.slice(0, -1) : url.pathname;
  url.pathname = `${existingPath}/instances/${instanceId}/cp`;

  if (url.protocol === 'https:') {
    url.protocol = 'wss:';
  } else if (url.protocol === 'http:') {
    url.protocol = 'ws:';
  }

  return url.toString();
}

/**
 * Copy a file or directory to a running instance.
 *
 * @example
 * ```typescript
 * import { cpToInstance } from 'hypeman/lib/cp';
 *
 * await cpToInstance({
 *   baseURL: 'https://api.hypeman.dev',
 *   apiKey: 'your-api-key',
 * }, {
 *   instanceId: 'inst_123',
 *   srcPath: './local-file.txt',
 *   dstPath: '/app/file.txt',
 * });
 * ```
 */
export async function cpToInstance(cfg: CpConfig, opts: CpToInstanceOptions): Promise<void> {
  // Get file stats
  const stats = fs.statSync(opts.srcPath);
  const isDir = stats.isDirectory();

  if (isDir) {
    // For directories, first create the directory, then recursively copy contents
    await cpSingleFileToInstance(cfg, {
      ...opts,
      isDir: true,
    });

    // Now recursively copy all contents
    const entries = fs.readdirSync(opts.srcPath, { withFileTypes: true });
    for (const entry of entries) {
      const srcEntryPath = path.join(opts.srcPath, entry.name);
      // Use path.posix.join for guest paths to ensure forward slashes
      const dstEntryPath = path.posix.join(opts.dstPath, entry.name);

      await cpToInstance(cfg, {
        instanceId: opts.instanceId,
        srcPath: srcEntryPath,
        dstPath: dstEntryPath,
        ...(opts.mode !== undefined && { mode: opts.mode }),
      });
    }
  } else {
    // For files, copy directly
    await cpSingleFileToInstance(cfg, {
      ...opts,
      isDir: false,
    });
  }
}

/**
 * Internal function to copy a single file or create a directory.
 */
async function cpSingleFileToInstance(
  cfg: CpConfig,
  opts: CpToInstanceOptions & { isDir: boolean },
): Promise<void> {
  // Check if WebSocket is available (Node.js vs browser)
  const WebSocket = getWebSocket();

  const wsURL = buildWsURL(cfg.baseURL, opts.instanceId);

  // Get file stats
  const stats = fs.statSync(opts.srcPath);
  const mode = opts.mode ?? stats.mode & 0o777;

  // Connect to WebSocket
  const ws = new WebSocket(wsURL, {
    headers: {
      Authorization: `Bearer ${cfg.apiKey}`,
    },
  });

  return new Promise((resolve, reject) => {
    let settled = false;
    let fileStream: fs.ReadStream | null = null;

    const doResolve = () => {
      if (!settled) {
        settled = true;
        resolve();
      }
    };

    const doReject = (err: Error) => {
      if (!settled) {
        settled = true;
        // Clean up file stream if open
        if (fileStream) {
          fileStream.destroy();
          fileStream = null;
        }
        reject(err);
      }
    };

    ws.on('open', () => {
      // Send initial request
      const req: CpRequest = {
        direction: 'to',
        guest_path: opts.dstPath,
        is_dir: opts.isDir,
        mode: mode,
      };
      ws.send(JSON.stringify(req));

      if (opts.isDir) {
        // For directories, just send end marker - the directory will be created
        ws.send(JSON.stringify({ type: 'end' }));
      } else {
        // Stream file content
        fileStream = fs.createReadStream(opts.srcPath, {
          highWaterMark: 32 * 1024,
        });

        fileStream.on('data', (chunk: any) => {
          ws.send(chunk as Buffer);
        });

        fileStream.on('end', () => {
          ws.send(JSON.stringify({ type: 'end' }));
        });

        fileStream.on('error', (err) => {
          ws.close();
          doReject(err);
        });
      }
    });

    ws.on('message', (data: Buffer | string) => {
      try {
        const msg = JSON.parse(data.toString()) as CpMessage;

        if (msg.type === 'result') {
          if (msg.success) {
            ws.close();
            doResolve();
          } else {
            ws.close();
            doReject(new Error(`Copy failed: ${msg.error}`));
          }
        } else if (msg.type === 'error') {
          ws.close();
          doReject(new Error(`Copy error at ${msg.path}: ${msg.message}`));
        }
      } catch {
        // Ignore non-JSON messages
      }
    });

    ws.on('error', (err) => {
      doReject(err);
    });

    ws.on('close', (code, reason) => {
      if (!settled) {
        if (code === 1000) {
          doReject(new Error('WebSocket closed before receiving result'));
        } else {
          doReject(new Error(`WebSocket closed: ${code} ${reason}`));
        }
      }
    });
  });
}

/**
 * Copy a file or directory from a running instance.
 *
 * @example
 * ```typescript
 * import { cpFromInstance } from 'hypeman/lib/cp';
 *
 * await cpFromInstance({
 *   baseURL: 'https://api.hypeman.dev',
 *   apiKey: 'your-api-key',
 * }, {
 *   instanceId: 'inst_123',
 *   srcPath: '/app/output.txt',
 *   dstPath: './local-output.txt',
 * });
 * ```
 */
export async function cpFromInstance(cfg: CpConfig, opts: CpFromInstanceOptions): Promise<void> {
  const WebSocket = getWebSocket();
  const wsURL = buildWsURL(cfg.baseURL, opts.instanceId);

  const ws = new WebSocket(wsURL, {
    headers: {
      Authorization: `Bearer ${cfg.apiKey}`,
    },
  });

  return new Promise((resolve, reject) => {
    let currentFile: fs.WriteStream | null = null;
    let currentHeader: CpFileHeader | null = null;
    let settled = false;

    const doResolve = () => {
      if (!settled) {
        settled = true;
        resolve();
      }
    };

    const doReject = (err: Error) => {
      if (!settled) {
        settled = true;
        reject(err);
      }
    };

    // Helper to close file and wait for it to complete
    const closeCurrentFile = (): Promise<void> => {
      return new Promise((res) => {
        if (currentFile) {
          currentFile.end(() => res());
        } else {
          res();
        }
      });
    };

    ws.on('open', () => {
      // Send initial request
      const req: CpRequest = {
        direction: 'from',
        guest_path: opts.srcPath,
        follow_links: opts.followLinks ?? false,
      };
      ws.send(JSON.stringify(req));
    });

    ws.on('message', (data: Buffer | string) => {
      // Don't process messages after the operation has settled
      if (settled) {
        return;
      }

      // Try to parse as JSON first
      let msg: CpMessage | null = null;
      if (typeof data === 'string' || (Buffer.isBuffer(data) && !isBinaryData(data))) {
        try {
          msg = JSON.parse(data.toString()) as CpMessage;
        } catch {
          // Not JSON, treat as binary data below
        }
      }

      // Handle parsed message (outside try-catch so FS errors propagate)
      if (msg) {
        try {
          switch (msg.type) {
            case 'header': {
              // Close previous file if any (synchronously for simplicity in message handler)
              if (currentFile) {
                currentFile.close();
                currentFile = null;
              }

              currentHeader = msg as CpFileHeader;
              // Sanitize server-provided path to prevent path traversal attacks
              const targetPath = sanitizePath(opts.dstPath, currentHeader.path);

              if (currentHeader.is_dir) {
                fs.mkdirSync(targetPath, { recursive: true, mode: currentHeader.mode });
              } else if (currentHeader.is_symlink) {
                // Handle symlink - ensure link_target is present
                if (!currentHeader.link_target) {
                  doReject(new Error(`Symlink ${currentHeader.path} missing link target`));
                  ws.close();
                  return;
                }
                // Validate symlink target
                const linkTarget = currentHeader.link_target;
                // Use path.posix.isAbsolute to detect Unix absolute paths even on Windows
                if (path.posix.isAbsolute(linkTarget) || path.isAbsolute(linkTarget)) {
                  doReject(new Error(`Invalid symlink target (absolute path): ${linkTarget}`));
                  ws.close();
                  return;
                }
                // Check if the symlink target escapes the destination directory
                // Resolve relative to the symlink's parent directory
                const linkDir = path.dirname(targetPath);
                const resolvedTarget = path.resolve(linkDir, linkTarget);
                const dstPathNorm = path.resolve(opts.dstPath);
                // For root destination, just ensure resolved target is absolute
                // For other destinations, ensure resolved target is within dstPath
                if (dstPathNorm !== '/' && dstPathNorm !== path.sep) {
                  if (!resolvedTarget.startsWith(dstPathNorm + path.sep) && resolvedTarget !== dstPathNorm) {
                    doReject(new Error(`Invalid symlink target (escapes destination): ${linkTarget}`));
                    ws.close();
                    return;
                  }
                }
                // Create parent directory if needed
                fs.mkdirSync(path.dirname(targetPath), { recursive: true });
                try {
                  fs.unlinkSync(targetPath);
                } catch {
                  // Ignore if doesn't exist
                }
                fs.symlinkSync(linkTarget, targetPath);
              } else {
                fs.mkdirSync(path.dirname(targetPath), { recursive: true });
                currentFile = fs.createWriteStream(targetPath, { mode: currentHeader.mode });
                // Add error handler to prevent unhandled 'error' events from crashing
                // Capture reference to avoid race condition if error fires after we've moved to next file
                const fileBeingWritten = currentFile;
                currentFile.on('error', (err) => {
                  fileBeingWritten.destroy();
                  // Only nullify currentFile if it's still pointing to this stream
                  // This prevents orphaning a subsequent file's stream if this error fires late
                  if (currentFile === fileBeingWritten) {
                    currentFile = null;
                  }
                  doReject(new Error(`Failed to write file ${targetPath}: ${err.message}`));
                  ws.close();
                });
              }
              break;
            }

            case 'end': {
              const endMsg = msg as CpEndMarker;
              if (currentFile) {
                const fileToClose = currentFile;
                const headerToUse = currentHeader;
                currentFile = null;
                currentHeader = null;

                // Wait for file to close before setting mtime
                fileToClose.end(() => {
                  if (headerToUse && headerToUse.mtime > 0) {
                    try {
                      const targetPath = sanitizePath(opts.dstPath, headerToUse.path);
                      const mtime = new Date(headerToUse.mtime * 1000);
                      fs.utimesSync(targetPath, mtime, mtime);
                    } catch (err) {
                      // Log but don't fail for mtime errors
                    }
                  }
                  if (endMsg.final) {
                    ws.close();
                    doResolve();
                  }
                });
                return; // Exit early since we handle resolution in callback
              }

              if (endMsg.final) {
                ws.close();
                doResolve();
              }
              break;
            }

            case 'error': {
              const errMsg = msg as CpError;
              ws.close();
              doReject(new Error(`Copy error at ${errMsg.path}: ${errMsg.message}`));
              break;
            }

            default:
              // Unrecognized JSON message type - log and ignore
              // This can happen if the protocol evolves with new message types
              break;
          }
        } catch (err) {
          // FS operation failed - propagate the error
          ws.close();
          doReject(err as Error);
        }
        return;
      }

      // Binary data - write to current file
      if (currentFile && Buffer.isBuffer(data)) {
        currentFile.write(data);
      }
    });

    ws.on('error', (err) => {
      if (currentFile) {
        currentFile.close();
      }
      doReject(err);
    });

    ws.on('close', (code, reason) => {
      if (currentFile) {
        currentFile.close();
      }
      if (!settled) {
        if (code === 1000) {
          doReject(new Error('WebSocket closed before receiving final marker'));
        } else {
          doReject(new Error(`WebSocket closed: ${code} ${reason}`));
        }
      }
    });
  });
}

/**
 * Helper to determine if data looks like binary (not JSON).
 */
function isBinaryData(data: Buffer): boolean {
  // If it starts with '{' or '[', it's likely JSON
  if (data.length > 0 && (data[0] === 0x7b || data[0] === 0x5b)) {
    return false;
  }
  return true;
}

/**
 * Get WebSocket implementation (works in Node.js).
 */
function getWebSocket(): typeof import('ws') {
  // In Node.js, we use the 'ws' package
  return require('ws');
}
