/// <reference path="../pb_data/types.d.ts" />
migrate((app) => {
  const collection = new Collection({
    name: "sprite_bases",
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
        name: "sheet",
        type: "file",
        required: true,
        maxSelect: 1,
        maxSize: 1048576,
        mimeTypes: ["image/png"],
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
  const collection = app.findCollectionByNameOrId("sprite_bases");
  return app.delete(collection);
});
