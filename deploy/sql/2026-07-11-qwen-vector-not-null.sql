-- Remove invalid readiness markers created by older workers, then prevent
-- subsequent NULL vectors from being counted as completed embeddings.

DO $$
BEGIN
    IF to_regclass('public.embeddings_v2') IS NOT NULL THEN
        DELETE FROM embeddings_v2 WHERE vector IS NULL;
        ALTER TABLE embeddings_v2 ALTER COLUMN vector SET NOT NULL;
    END IF;

    IF to_regclass('public.chunk_embeddings_v2') IS NOT NULL THEN
        DELETE FROM chunk_embeddings_v2 WHERE vector IS NULL;
        ALTER TABLE chunk_embeddings_v2 ALTER COLUMN vector SET NOT NULL;
    END IF;
END
$$;
