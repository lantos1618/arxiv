#!/usr/bin/env python3
"""
Generate embedding for a single query.
Used by Go server for semantic search queries.
"""

import sys
import json
import os

# Add tools directory to path
sys.path.append('/home/ubuntu/arxiv/tools')

try:
    from sentence_transformers import SentenceTransformer
    import numpy as np
    
    # Load model (cached globally)
    model = SentenceTransformer('all-MiniLM-L6-v2')
    
    # Get query from environment variable or command line
    query = os.environ.get('QUERY') or (sys.argv[1] if len(sys.argv) > 1 else '')
    
    if not query:
        print("ERROR: No query provided", file=sys.stderr)
        sys.exit(1)
    
    # Generate embedding
    embedding = model.encode([query], convert_to_numpy=True)[0]
    
    # Output as comma-separated values for easy parsing
    print(','.join(map(str, embedding.astype(np.float32))))
    
except ImportError as e:
    print(f"ERROR: Missing dependency - {e}", file=sys.stderr)
    print("Please install with: pip install sentence-transformers numpy", file=sys.stderr)
    sys.exit(1)
except Exception as e:
    print(f"ERROR: {e}", file=sys.stderr)
    sys.exit(1)