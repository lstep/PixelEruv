/// <reference path="../pb_data/types.d.ts" />
migrate((app) => {
  const collection = new Collection({
    name: "maps",
    type: "base",
    fields: [
      {
        name: "name",
        type: "text",
        required: true,
        min: 1,
        max: 100,
      },
      {
        name: "tiled_json",
        type: "file",
        required: true,
        maxSelect: 1,
        maxSize: 5242880,
        mimeTypes: ["application/json"],
      },
      {
        name: "tilesets",
        type: "file",
        required: true,
        maxSelect: 10,
        maxSize: 10485760,
        mimeTypes: ["image/png", "image/jpeg"],
      },
    ],
    listRule: "",
    viewRule: "",
    createRule: null,
    updateRule: null,
    deleteRule: null,
  });

  return app.save(collection);
}, (app) => {
  const collection = app.findCollectionByNameOrId("maps");
  return app.delete(collection);
});
