package database

import "context"

// GetImageGenerationJobResult reads an owner's job without loading prompts,
// expanded input images, or API key display metadata. The full job API is unchanged.
func (db *DB) GetImageGenerationJobResult(ctx context.Context, id, apiKeyID int64) (*ImageGenerationJob, error) {
	job, err := scanImageGenerationJob(db.conn.QueryRowContext(ctx, `
		SELECT id, status, '', '', api_key_id, '', '', error_message,
			duration_ms, created_at, started_at, completed_at
		FROM image_generation_jobs WHERE id=$1 AND api_key_id=$2
	`, id, apiKeyID))
	if err != nil {
		return nil, err
	}
	job.Assets, err = db.ListImageAssetsByJobID(ctx, id)
	if err != nil {
		return nil, err
	}
	return job, nil
}
