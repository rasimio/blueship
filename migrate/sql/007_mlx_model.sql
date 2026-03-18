-- MLX local model (OpenAI-compatible)
INSERT INTO available_models (provider, name, display_name, capabilities) VALUES
    ('mlx', 'mlx-community/Qwen3.5-35B-A3B-4bit', 'Qwen3.5 35B (MLX local)', '{primary,compact}')
ON CONFLICT DO NOTHING;
